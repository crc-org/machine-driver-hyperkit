# Go and compilation related variables
BUILD_DIR ?= out
GOPATH ?= $(shell go env GOPATH)

ORG := github.com/machine-drivers
REPOPATH ?= $(ORG)/docker-machine-driver-hyperkit
GOLANGCI_LINT_VERSION=v1.43.0

default: build

.PHONY: vendor
vendor:
	go mod tidy
	go mod vendor

$(BUILD_DIR):
	mkdir -p $(BUILD_DIR)

.PHONY: clean
clean:
	rm -rf $(BUILD_DIR)
	rm -rf vendor

.PHONY: build
build: $(BUILD_DIR) vendor lint test
	GOOS=darwin go build \
			-installsuffix "static" \
			-ldflags="-s -w" \
			-o $(BUILD_DIR)/crc-driver-hyperkit
	chmod +x $(BUILD_DIR)/crc-driver-hyperkit

.PHONY: golangci-lint
golangci-lint:
	@if $(GOPATH)/bin/golangci-lint version 2>&1 | grep -vq $(GOLANGCI_LINT_VERSION); then\
		pushd /tmp && GO111MODULE=on go get github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) && popd; \
	fi

.PHONY: lint
lint: golangci-lint
	$(GOPATH)/bin/golangci-lint run

.PHONY: test
test:
	go test ./...

.PHONY: vendorcheck
vendorcheck:
	./verify-vendor.sh
