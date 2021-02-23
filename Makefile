# Go and compilation related variables
BUILD_DIR ?= out

ORG := github.com/machine-drivers
REPOPATH ?= $(ORG)/docker-machine-driver-hyperkit

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

.PHONY: lint
lint:
	golangci-lint run

.PHONY: test
test:
	go test ./...

.PHONY: vendorcheck
vendorcheck:
	./verify-vendor.sh
