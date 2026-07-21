# Makefile for cube-bench

BINARY  := bin/cube-bench
GOFLAGS ?=

.PHONY: build clean fmt help

## build: compile the benchmark binary (default)
build: $(BINARY)

$(BINARY):
	@mkdir -p bin
	go build $(GOFLAGS) -o $(BINARY) .
	@echo "Built: $(BINARY)"

## fmt: format Go sources
fmt:
	go fmt ./...

## clean: remove compiled binary
clean:
	rm -rf bin

## help: list available targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'

.DEFAULT_GOAL := build
