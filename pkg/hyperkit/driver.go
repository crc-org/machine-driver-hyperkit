// +build darwin

/*
Copyright 2016 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package hyperkit

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	hyperkitdriver "github.com/code-ready/machine/drivers/hyperkit"
	"github.com/code-ready/machine/libmachine/drivers"
	"github.com/code-ready/machine/libmachine/state"
	"github.com/mitchellh/go-ps"
	hyperkit "github.com/moby/hyperkit/go"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

const (
	pidFileName = "hyperkit.pid"
	permErr     = "%s needs to run with elevated permissions. " +
		"Please run the following command, then try again: " +
		"sudo chown root:wheel %s && sudo chmod u+s %s"
)

type Driver hyperkitdriver.Driver

// NewDriver creates a new driver for a host
func NewDriver() *Driver {
	return &Driver{
		VMDriver: &drivers.VMDriver{
			BaseDriver: &drivers.BaseDriver{},
			CPU:        DefaultCPUs,
			Memory:     DefaultMemory,
		},
	}
}

// PreCreateCheck is called to enforce pre-creation steps
func (d *Driver) PreCreateCheck() error {
	return d.verifyRootPermissions()
}

// verifyRootPermissions is called before any step which needs root access
func (d *Driver) verifyRootPermissions() error {
	if !d.VMNet {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	euid := syscall.Geteuid()
	log.Debugf("exe=%s uid=%d", exe, euid)
	if euid != 0 {
		return fmt.Errorf(permErr, filepath.Base(exe), exe, exe)
	}
	return nil
}

func (d *Driver) getDiskPath() string {
	return d.ResolveStorePath(fmt.Sprintf("%s.%s", d.MachineName, d.ImageFormat))
}

// Create a host using the driver's config
func (d *Driver) Create() error {
	if err := d.verifyRootPermissions(); err != nil {
		return err
	}

	if err := copyFile(d.ImageSourcePath, d.getDiskPath()); err != nil {
		return err
	}

	return d.Start()
}

// DriverName returns the name of the driver
func (d *Driver) DriverName() string {
	return DriverName
}

// GetSSHHostname returns hostname for use with ssh
func (d *Driver) GetSSHHostname() (string, error) {
	return d.IPAddress, nil
}

// GetState returns the state that the host is in (running, stopped, etc)
func (d *Driver) GetState() (state.State, error) {
	if err := d.verifyRootPermissions(); err != nil {
		return state.Error, err
	}

	p, err := d.findHyperkitProcess()
	if err != nil {
		return state.Error, err
	}
	if p == nil {
		return state.Stopped, nil
	}
	return state.Running, nil
}

// Kill stops a host forcefully
func (d *Driver) Kill() error {
	if err := d.verifyRootPermissions(); err != nil {
		return err
	}
	return d.sendSignal(syscall.SIGKILL)
}

// Remove a host
func (d *Driver) Remove() error {
	if err := d.verifyRootPermissions(); err != nil {
		return err
	}

	s, err := d.GetState()
	if err != nil || s == state.Error {
		log.Debugf("Error checking machine status: %v, assuming it has been removed already", err)
	}
	if s == state.Running {
		if err := d.Kill(); err != nil {
			return err
		}
	}
	return nil
}

// Restart a host
func (d *Driver) Restart() error {
	if err := d.Stop(); err != nil {
		return err
	}

	return d.Start()
}

// Start a host
func (d *Driver) Start() error {
	if err := d.verifyRootPermissions(); err != nil {
		return err
	}

	stateDir := d.ResolveStorePath("")
	if err := d.recoverFromUncleanShutdown(); err != nil {
		return err
	}
	h, err := hyperkit.New(d.HyperKitPath, d.VpnKitSock, stateDir)
	if err != nil {
		return errors.Wrap(err, "new-ing Hyperkit")
	}
	log.Debugf("Using hyperkit binary from %s", h.HyperKit)
	// TODO: handle the rest of our settings.
	h.Kernel = d.VmlinuzPath
	h.Initrd = d.InitrdPath
	h.VMNet = d.VMNet
	h.Console = hyperkit.ConsoleFile
	h.CPUs = d.CPU
	h.Memory = d.Memory
	h.UUID = d.UUID
	h.VSock = true
	h.VSockGuestCID = 3

	if vsockPorts, err := d.extractVSockPorts(); err != nil {
		return err
	} else if len(vsockPorts) >= 1 {
		h.VSockPorts = vsockPorts
	}

	mac := ""
	if d.VMNet {
		var err error
		log.Debugf("Using UUID %s", h.UUID)
		mac, err = GetMACAddressFromUUID(h.UUID)
		if err != nil {
			return errors.Wrap(err, "getting MAC address from UUID")
		}

		// Need to strip 0's
		mac = trimMacAddress(mac)
		log.Debugf("Generated MAC %s", mac)
	}

	if d.ImageFormat != "qcow2" {
		return fmt.Errorf("Unsupported VM image format: %s", d.ImageFormat)
	}
	h.Disks = []hyperkit.DiskConfig{
		{
			Path:   fmt.Sprintf("file://%s", d.getDiskPath()),
			Driver: "virtio-blk",
			Format: "qcow",
		},
	}
	log.Debugf("Starting with cmdline: %s", d.Cmdline)
	if err := h.Start(d.Cmdline); err != nil {
		log.Debugf("Error trying to execute %s", h.CmdLine)
		return errors.Wrapf(err, "starting with cmd line: %s", d.Cmdline)
	}

	log.Debugf("Trying to execute %s", h.CmdLine)

	waitUntilRunning := func() error {
		st, err := d.GetState()
		if err != nil {
			return err
		}
		if st == state.Running {
			return nil
		}
		return &RetriableError{fmt.Errorf("hyperkit not running yet")}
	}
	if err := RetryAfter(5, waitUntilRunning, time.Second); err != nil {
		return fmt.Errorf("VM failed to start: %v", err)
	}

	if !d.VMNet {
		return nil
	}

	getIP := func() error {
		d.IPAddress, err = GetIPAddressByMACAddress(mac)
		if err != nil {
			return &RetriableError{Err: err}
		}
		return nil
	}

	if err := RetryAfter(60, getIP, 2*time.Second); err != nil {
		return fmt.Errorf("IP address never found in dhcp leases file %v", err)
	}
	log.Debugf("IP: %s", d.IPAddress)

	return nil
}

// GetURL is not implemented yet
func (d *Driver) GetURL() (string, error) {
	return "", nil
}

func (d *Driver) DriverVersion() string {
	return DriverVersion
}

// recoverFromUncleanShutdown searches for an existing hyperkit.pid file in
// the machine directory. If it can't find it, a clean shutdown is assumed.
// If it finds the pid file, it checks for a running hyperkit process with that pid
// as the existence of a file might not indicate an unclean shutdown but an actual running
// hyperkit server. This is an error situation - we shouldn't start minikube as there is likely
// an instance running already. If the PID in the pidfile does not belong to a running hyperkit
// process, we can safely delete it, and there is a good chance the machine will recover when restarted.
func (d *Driver) recoverFromUncleanShutdown() error {
	proc, err := d.findHyperkitProcess()
	if err == nil && proc != nil {
		/* hyperkit is running, pid file can't be stale */
		return nil
	}
	/* There might be a stale pid file, try to remove it */
	pidFile := d.ResolveStorePath(pidFileName)
	if err := os.Remove(pidFile); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return errors.Wrap(err, fmt.Sprintf("removing pidFile %s", pidFile))
		}
	} else {
		log.Debugf("Removed stale pid file %s...", pidFile)
	}
	return nil
}

// Stop a host gracefully
func (d *Driver) Stop() error {
	if err := d.verifyRootPermissions(); err != nil {
		return err
	}

	s, err := d.GetState()
	if err != nil {
		return err
	}

	if s != state.Stopped {
		err := d.sendSignal(syscall.SIGTERM)
		if err != nil {
			return errors.Wrap(err, "hyperkit sigterm failed")
		}
		// wait 120s for graceful shutdown
		for i := 0; i < 60; i++ {
			time.Sleep(2 * time.Second)
			s, _ := d.GetState()
			log.Debugf("VM state: %s", s)
			if s == state.Stopped {
				return nil
			}
		}
		return errors.New("VM Failed to gracefully shutdown, try the kill command")
	}
	return nil
}

// InvalidPortNumberError implements the Error interface.
// It is used when a VSockPorts port number cannot be recognised as an integer.
type InvalidPortNumberError string

// Error returns an Error for InvalidPortNumberError
func (port InvalidPortNumberError) Error() string {
	return fmt.Sprintf("vsock port '%s' is not an integer", string(port))
}

func (d *Driver) extractVSockPorts() ([]int, error) {
	vsockPorts := make([]int, 0, len(d.VSockPorts))

	for _, port := range d.VSockPorts {
		p, err := strconv.Atoi(port)
		if err != nil {
			return nil, InvalidPortNumberError(port)
		}
		vsockPorts = append(vsockPorts, p)
	}

	return vsockPorts, nil
}

func (d *Driver) sendSignal(s os.Signal) error {
	psProc, err := d.findHyperkitProcess()
	if err != nil {
		return err
	}
	proc, err := os.FindProcess(psProc.Pid())
	if err != nil {
		return err
	}

	return proc.Signal(s)
}

func readPidFromFile(filename string) (int, error) {
	bs, err := ioutil.ReadFile(filename)
	if err != nil {
		return 0, err
	}
	content := strings.TrimSpace(string(bs))
	pid, err := strconv.Atoi(content)
	if err != nil {
		return 0, errors.Wrapf(err, "parsing %s", filename)
	}

	return pid, nil
}

/*
 * Returns a ps.Process instance if it could find a hyperkit process with the pid
 * stored in $pidFileName
 *
 * Returns nil, nil if:
 * - if the $pidFileName file does not exist,
 * - if a process with the pid from this file cannot be found,
 * - if a process was found, but its name does not contain 'hyper'
 */
func (d *Driver) findHyperkitProcess() (ps.Process, error) {
	pidFile := d.ResolveStorePath(pidFileName)

	pid, err := readPidFromFile(pidFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, errors.Wrapf(err, "error reading pidfile %s", pidFile)
	}

	p, err := ps.FindProcess(pid)
	if err != nil {
		return nil, errors.Wrap(err, fmt.Sprintf("cannot find pid %d", pid))
	}
	if p == nil {
		log.Debugf("hyperkit pid %d missing from process table", pid)
		// return PidNotExist error?
		return nil, nil
	}

	// match both hyperkit and com.docker.hyper
	if !strings.Contains(p.Executable(), "hyper") {
		// return InvalidExecutable error?
		log.Debugf("pid %d is stale, and is being used by %s", pid, p.Executable())
		return nil, nil
	}

	return p, nil
}

func (d *Driver) UpdateConfigRaw(rawConfig []byte) error {
	var newDriver Driver
	err := json.Unmarshal(rawConfig, &newDriver)
	if err != nil {
		return err
	}

	if newDriver.Memory == d.Memory && newDriver.CPU == d.CPU {
		/* For now only changing memory and CPU is supported/tested.
		 * If none of these changed, we might be trying to change another
		 * value, which is may or may not work, return ErrNotImplemented for now
		 */
		return drivers.ErrNotImplemented
	}
	*d = newDriver

	return nil
}
