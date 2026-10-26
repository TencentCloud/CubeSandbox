# Makefile for the S3-compatible Volume plugin (cube-volume-s3)
#
# The plugin is a static binary invoked once per hook by CubeMaster / Cubelet.
# The repo root Makefile builds it into _output/bin via `make cube-volume-s3`.

BINARY   := bin/cube-volume-s3
GOFLAGS  ?=
LDFLAGS  ?= -s -w
SOURCES  := $(shell find . -name '*.go' -not -name '*_test.go')

.PHONY: build clean fmt lint test help

## build: compile the plugin binary (default)
build: $(BINARY)

# CGO is disabled so the binary runs on any glibc/musl host regardless of the
# builder image, matching how the other Go components are packaged.
$(BINARY): $(SOURCES) go.mod go.sum
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=$(shell go env GOARCH) \
		go build $(GOFLAGS) -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/cube-volume-s3
	@echo "Built: $(BINARY)"

## fmt: format Go sources
fmt:
	go fmt ./...

## lint: vet Go sources
lint:
	go vet ./...

## test: run unit tests (no cloud access required)
test:
	go test ./...

## clean: remove compiled binary
clean:
	rm -rf bin

## help: list available targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'

.DEFAULT_GOAL := build
