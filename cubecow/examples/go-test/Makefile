# cubecow Go FFI Test - Makefile
#
# Prerequisites:
#   1. Build libcubecow.so: cd ../../ && cargo build --release
#   2. Go 1.21+

# Path to the Rust project root (relative to this Makefile)
RUST_PROJECT := ../..
LIB_DIR := $(RUST_PROJECT)/target/release

# Shared library name
LIB_NAME := cubecow

# Go build flags: tell CGO where to find the header and library
export CGO_CFLAGS := -I$(CURDIR)/cubecow
export CGO_LDFLAGS := -L$(abspath $(LIB_DIR)) -l$(LIB_NAME) -Wl,-rpath,$(abspath $(LIB_DIR))

# Output binary
BINARY := cubecow-test

.PHONY: all build run clean rust-lib help

all: build

## Build the Rust shared library first
rust-lib:
	@echo ">>> Building libcubecow.so ..."
	cd $(RUST_PROJECT) && cargo build --release
	@echo ">>> Rust library built: $(LIB_DIR)/lib$(LIB_NAME).so"

## Build the Go test binary
build: rust-lib
	@echo ">>> Building Go test binary ..."
	go build -o $(BINARY) .
	@echo ">>> Built: ./$(BINARY)"

## Build Go binary only (skip Rust build, assumes lib already exists)
build-go:
	@echo ">>> Building Go test binary (lib must exist) ..."
	go build -o $(BINARY) .
	@echo ">>> Built: ./$(BINARY)"

## Verify the shared library exists and exports the expected symbols.
##
## NOTE: function names retain the historical `cubecow_` prefix so
## existing C/Go consumers keep working — that's why the grep below
## still matches `cubecow_`. The lib name itself is `libcubecow.so`.
check-lib:
	@echo ">>> Checking $(LIB_DIR)/lib$(LIB_NAME).so ..."
	@test -f $(LIB_DIR)/lib$(LIB_NAME).so || \
		(echo "ERROR: $(LIB_DIR)/lib$(LIB_NAME).so not found. Run 'make rust-lib' first." && exit 1)
	@echo ">>> Checking exported FFI symbols ..."
	@nm -D --defined-only $(LIB_DIR)/lib$(LIB_NAME).so | grep -E ' T cubecow_' | sort
	@echo ">>> OK"

## Run `go vet` against the cgo package (requires the lib + header to resolve)
vet: rust-lib
	@echo ">>> Running go vet ..."
	go vet ./...

## Run the full lifecycle test
run: build
	@echo ">>> Running full test ..."
	LD_LIBRARY_PATH=$(abspath $(LIB_DIR)):$$LD_LIBRARY_PATH ./$(BINARY) -action all

## Run with a specific action
run-action: build
	@echo ">>> Running action: $(ACTION) ..."
	LD_LIBRARY_PATH=$(abspath $(LIB_DIR)):$$LD_LIBRARY_PATH ./$(BINARY) -action $(ACTION)

## Clean build artifacts
clean:
	rm -f $(BINARY)

## Show help
help:
	@echo "cubecow Go FFI Test"
	@echo ""
	@echo "Targets:"
	@echo "  make build       - Build Rust lib + Go binary"
	@echo "  make build-go    - Build Go binary only (assumes lib exists)"
	@echo "  make run         - Build and run full lifecycle test"
	@echo "  make run-action ACTION=<action>  - Run a specific action"
	@echo "  make check-lib   - Verify libcubecow.so exists and list exported FFI symbols"
	@echo "  make vet         - Run 'go vet' against the cgo wrapper"
	@echo "  make clean       - Remove build artifacts"
	@echo "  make rust-lib    - Build Rust shared library only"
	@echo ""
	@echo "Actions: init, create-vol, delete-vol, resize-vol, vol-info,"
	@echo "         list-vols, create-snap, delete-snap, list-snaps,"
	@echo "         list-pools, metrics, all"
	@echo ""
	@echo "Examples:"
	@echo "  make run"
	@echo "  make run-action ACTION=list-pools"
	@echo "  make run-action ACTION=create-vol"
