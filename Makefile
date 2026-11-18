BIN := bin/ipmi-connect

# Prefer the copy in $GOPATH/bin: golangci-lint must be built with a Go at least as new as go.mod.
GOLANGCI_LINT ?= $(firstword $(wildcard $(shell go env GOPATH)/bin/golangci-lint) golangci-lint)

.PHONY: all build desktop test lint fmt clean

all: lint test build

build:
	go build -o $(BIN) ./cmd/ipmi-connect

# Spike: the console in a native Wails window (GTK4 + WebKitGTK 6.0, loaded at run time, so no
# cgo). See docs/design/wails-spike.md.
desktop:
	CGO_ENABLED=0 go build -tags desktop,purego -o bin/ipmi-connect-desktop ./cmd/ipmi-connect

test:
	go test -race ./...

lint:
	go vet ./...
	$(GOLANGCI_LINT) run ./...

fmt:
	$(GOLANGCI_LINT) fmt ./...

clean:
	rm -rf bin
