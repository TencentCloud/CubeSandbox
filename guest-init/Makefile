# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

PROJECT_COMPONENT = cube-init
TARGET = $(PROJECT_COMPONENT)

BUILD_TYPE = release
MUSL := yes
HOST_ARCH := $(shell uname -m | sed 's/^arm64$$/aarch64/')

EXTRA_RUSTFLAGS :=
CARGO_TARGET_FLAG :=

ifeq ($(MUSL),yes)
    TRIPLE ?= $(HOST_ARCH)-unknown-linux-musl
    CARGO_TARGET_FLAG := --target $(TRIPLE)
    override EXTRA_RUSTFLAGS += -C target-feature=+crt-static -C relocation-model=static -C link-arg=-static
    ifeq ($(TRIPLE),x86_64-unknown-linux-musl)
        export CARGO_TARGET_X86_64_UNKNOWN_LINUX_MUSL_LINKER ?= musl-gcc
    endif
    ifeq ($(TRIPLE),aarch64-unknown-linux-musl)
        export CARGO_TARGET_AARCH64_UNKNOWN_LINUX_MUSL_LINKER ?= musl-gcc
    endif
endif

ifeq ($(MUSL),yes)
TARGET_PATH = target/$(TRIPLE)/$(BUILD_TYPE)/$(TARGET)
else
TARGET_PATH = target/$(BUILD_TYPE)/$(TARGET)
endif

DESTDIR :=
BINDIR := /usr/bin

.DEFAULT_GOAL := default

##TARGET default: build cube-init
default: $(TARGET_PATH)

$(TARGET_PATH):
	@RUSTFLAGS="$(EXTRA_RUSTFLAGS) --deny warnings" cargo build $(if $(findstring release,$(BUILD_TYPE)),--release) $(CARGO_TARGET_FLAG)

##TARGET install: install cube-init
install:
	@install -D $(TARGET_PATH) $(DESTDIR)/$(BINDIR)/$(TARGET)

##TARGET clean: clean build
clean:
	@cargo clean

##TARGET test: run tests
test:
	@cargo test --all -- --nocapture

##TARGET fmt: format code
fmt:
	cargo fmt

.PHONY: default install clean test fmt
