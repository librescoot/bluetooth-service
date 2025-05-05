.PHONY: build clean build-arm build-amd64 lint test

BINARY_NAME=bluetooth-service
BUILD_DIR=bin
LDFLAGS=-ldflags "-w -s -extldflags '-static'"

build:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=6 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/bluetooth-service

clean:
	rm -rf $(BUILD_DIR)

build-arm:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=6 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/bluetooth-service

build-amd64:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-amd64 ./cmd/bluetooth-service

lint:
	golangci-lint run

test:
	go test -v ./... 
