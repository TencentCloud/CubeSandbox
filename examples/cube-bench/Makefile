# Makefile for cube-bench

BINARY  := bin/cube-bench
GOFLAGS ?=

.PHONY: build clean help

## build: compile the benchmark binary (default)
build: $(BINARY)

$(BINARY):
	@mkdir -p bin
	go build $(GOFLAGS) -o $(BINARY) .
	@echo "Built: $(BINARY)"

## clean: remove compiled binary
clean:
	rm -rf bin

## help: list available targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'

.DEFAULT_GOAL := build
