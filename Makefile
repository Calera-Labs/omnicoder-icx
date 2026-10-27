# OmniCoder-ICX Makefile
BINARY_NAME := omnicoder
PKG := ./...
LDFLAGS := -s -w

.PHONY: all build test test-race vet release clean

all: vet test build

build:
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -trimpath -o bin/$(BINARY_NAME) ./cmd/omnicoder

vet:
	go vet $(PKG)

test:
	go test -count=1 $(PKG)

test-race:
	go test -race -count=1 ./pkg/storage/... ./pkg/engine/... ./pkg/icx/... ./pkg/mcp/...

release:
	powershell -ExecutionPolicy Bypass -File scripts/build_release.ps1

clean:
	rm -rf bin/ release/ *.coverprofile
