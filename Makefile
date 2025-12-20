.PHONY: build clean dist build-arm build-amd64 build-host lint test fmt deps

BINARY_NAME=bluetooth-service
BUILD_DIR=bin
VERSION=$(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS=-ldflags "-w -s -X main.version=$(VERSION) -extldflags '-static'"

build:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/bluetooth-service

dist:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/bluetooth-service

build-arm:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/bluetooth-service

clean:
	rm -rf $(BUILD_DIR)

build-amd64:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-amd64 ./cmd/bluetooth-service

lint:
	golangci-lint run

test:
	go test -v ./...

build-host:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-host ./cmd/bluetooth-service

fmt:
	go fmt ./...

deps:
	go mod download && go mod tidy
