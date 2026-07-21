# Copyright (c) 2019 Intel Corporation
#
# SPDX-License-Identifier: Apache-2.0
#

PROJECT_NAME = Cube Containers
PROJECT_URL = https://github.com/tencentcloud/CubeSandbox
PROJECT_COMPONENT = cube-agent

TARGET = $(PROJECT_COMPONENT)

DOCKER_IMAGE_VERSION = 

DOCKER_GIT_CRED =
ifneq ($(wildcard $(HOME)/.git-credentials),)
DOCKER_GIT_CRED += -v $(HOME)/.git-credentials:/.git-credentials:ro
endif

UID = $(shell id -u)
GID = $(shell id -g)
PWD = $(shell pwd)

VERSION := $(if $(strip $(CUBE_VERSION)),$(CUBE_VERSION),$(shell git describe --abbrev=0 --tags --match 'v*' 2>/dev/null || echo "0.0.0-dev"))
COMMIT_NO := $(if $(strip $(CUBE_COMMIT)),$(CUBE_COMMIT),$(shell git rev-parse HEAD 2>/dev/null || true))
COMMIT := $(if $(strip $(CUBE_COMMIT)),$(CUBE_COMMIT),$(if $(shell git status --porcelain --untracked-files=no 2>/dev/null || true),${COMMIT_NO}-dirty,${COMMIT_NO}))
COMMIT_MSG = $(if $(COMMIT),$(COMMIT),unknown)
BUILD_TIME := $(if $(strip $(CUBE_BUILD_TIME)),$(CUBE_BUILD_TIME),$(shell date -u +'%Y-%m-%dT%H:%M:%SZ'))

export VERSION_COMMIT := $(if $(COMMIT),$(COMMIT),$(VERSION))

EXTRA_RUSTFEATURES :=

##VAR SECCOMP=yes|no define if agent enables seccomp feature
SECCOMP := yes
ifeq ($(SECCOMP),yes)
    override EXTRA_RUSTFEATURES += seccomp
endif

##VAR STANDARD_OCI_RUNTIME=yes|no define if agent enables standard oci runtime feature
STANDARD_OCI_RUNTIME := no
ifeq ($(STANDARD_OCI_RUNTIME),yes)
    override EXTRA_RUSTFEATURES += standard-oci-runtime
endif

ifneq ($(EXTRA_RUSTFEATURES),)
    override EXTRA_RUSTFEATURES := --features "$(EXTRA_RUSTFEATURES)"
endif

##VAR BUILD_TYPE=release|debug type of rust build
BUILD_TYPE = release

##VAR MUSL=yes|no build a fully static native musl binary
MUSL := yes
HOST_ARCH := $(shell uname -m | sed 's/^arm64$$/aarch64/')

EXTRA_RUSTFLAGS :=
CARGO_TARGET_FLAG :=

ifeq ($(MUSL),yes)
    TRIPLE ?= $(HOST_ARCH)-unknown-linux-musl
    CARGO_TARGET_FLAG := --target $(TRIPLE)
    # +crt-static: statically link libc/crt
    # -lpthread: required by libunwind in musl builds (pthread_rwlock_*)
    # -lm: keeps libseccomp/libm references (for example pow) resolved by musl libm
    override EXTRA_RUSTFLAGS += -C target-feature=+crt-static -C relocation-model=static -C link-arg=-static -C link-arg=-lpthread -C link-arg=-lm
    # Keep cargo on the musl linker even when the host default cc points at glibc.
    ifeq ($(TRIPLE),x86_64-unknown-linux-musl)
        export CARGO_TARGET_X86_64_UNKNOWN_LINUX_MUSL_LINKER ?= musl-gcc
    endif
    ifeq ($(TRIPLE),aarch64-unknown-linux-musl)
        export CARGO_TARGET_AARCH64_UNKNOWN_LINUX_MUSL_LINKER ?= musl-gcc
    endif
    # Static libseccomp must be built with musl as well. Do not fall back to
    # pkg-config or /usr/lib/*-linux-gnu here: those paths often resolve to
    # glibc archives, producing a binary that fails as PID 1 in the guest.
    MUSL_LIBSECCOMP_LIB_PATH := $(or \
        $(LIBSECCOMP_LIB_PATH), \
        $(shell test -f /usr/local/lib64/libseccomp/lib/libseccomp.a && echo /usr/local/lib64/libseccomp/lib), \
        $(shell test -f /usr/lib/$(HOST_ARCH)-linux-musl/libseccomp.a && echo /usr/lib/$(HOST_ARCH)-linux-musl), \
        $(shell test -f /usr/lib/$(TRIPLE)/libseccomp.a && echo /usr/lib/$(TRIPLE)))
    ifeq ($(SECCOMP),yes)
        ifeq ($(strip $(MUSL_LIBSECCOMP_LIB_PATH)),)
            $(error MUSL=yes SECCOMP=yes requires a musl-built static libseccomp.a. Run 'make builder-image && make agent', or set LIBSECCOMP_LIB_PATH=/path/to/musl/libseccomp/lib)
        endif
        ifneq ($(findstring linux-gnu,$(MUSL_LIBSECCOMP_LIB_PATH)),)
            $(error LIBSECCOMP_LIB_PATH=$(MUSL_LIBSECCOMP_LIB_PATH) looks like a glibc libdir. Use the builder image or a musl-built static libseccomp.a)
        endif
        ifneq ($(filter /usr/lib /usr/lib64,$(MUSL_LIBSECCOMP_LIB_PATH)),)
            $(error LIBSECCOMP_LIB_PATH=$(MUSL_LIBSECCOMP_LIB_PATH) is too broad for a musl static build. Point it at a musl-specific libseccomp directory)
        endif
        ifeq ($(wildcard $(MUSL_LIBSECCOMP_LIB_PATH)/libseccomp.a),)
            $(error LIBSECCOMP_LIB_PATH=$(MUSL_LIBSECCOMP_LIB_PATH) does not contain libseccomp.a)
        endif
        export LIBSECCOMP_LINK_TYPE := static
        export LIBSECCOMP_LIB_PATH := $(MUSL_LIBSECCOMP_LIB_PATH)
    endif
endif

CWD := $(shell dirname $(realpath $(firstword $(MAKEFILE_LIST))))

ifeq ($(MUSL),yes)
TARGET_PATH = target/$(TRIPLE)/$(BUILD_TYPE)/$(TARGET)
else
TARGET_PATH = target/$(BUILD_TYPE)/$(TARGET)
endif

##VAR DESTDIR=<path> is a directory prepended to each installed target file
DESTDIR :=
##VAR BINDIR=<path> is a directory for installing executable programs
BINDIR := /usr/bin

GENERATED_CODE = src/version.rs
AGENT_NAME = $(TARGET)
API_VERSION = 0.0.1
AGENT_VERSION = $(VERSION)

GENERATED_REPLACEMENTS = AGENT_NAME AGENT_VERSION API_VERSION BINDIR COMMIT VERSION_COMMIT BUILD_TIME
GENERATED_FILES = $(GENERATED_CODE)

define get_command_version
$(shell printf "%s: %s\\n" $(1) "$(or $(shell $(1) --version 2>/dev/null), (not available))")
endef

define get_toolchain_version
$(shell printf "%s: %s\\n" "toolchain" "$(or $(shell rustup show active-toolchain 2>/dev/null), (unknown))")
endef

.DEFAULT_GOAL := default

##TARGET default: build code
default: $(TARGET) show-header

$(TARGET): $(GENERATED_CODE) logging-crate-tests $(TARGET_PATH)

logging-crate-tests:
	$(MAKE) -C $(CWD)/libs/logging

$(TARGET_PATH): show-summary
	@RUSTFLAGS="$(EXTRA_RUSTFLAGS) --deny warnings" cargo build $(if $(findstring release,$(BUILD_TYPE)),--release) $(CARGO_TARGET_FLAG) $(EXTRA_RUSTFEATURES)

$(GENERATED_FILES): %: %.in FORCE
	@sed $(foreach r,$(GENERATED_REPLACEMENTS),-e 's|@$r@|$($r)|g') "$<" > "$@"

##TARGET optimize: optimized build
optimize: show-summary show-header
	@RUSTFLAGS="-C link-arg=-s $(EXTRA_RUSTFLAGS) --deny warnings" cargo build $(if $(findstring release,$(BUILD_TYPE)),--release) $(CARGO_TARGET_FLAG) $(EXTRA_RUSTFEATURES)

##TARGET install: install agent
install:
	@install -D $(TARGET_PATH) $(DESTDIR)/$(BINDIR)/$(TARGET)

##TARGET clean: clean build
clean:
	@cargo clean
	@rm -f $(GENERATED_FILES)

##TARGET test: run tests
test:
	@cargo test --all $(EXTRA_RUSTFEATURES) -- --nocapture

##TARGET fmt: format code
fmt: $(GENERATED_FILES)
	@if [ ! -f libs/protocols/src/agent.rs ]; then cargo check -p protocols; fi
	cargo fmt

##TARGET check: run lint/fmt check
check: $(GENERATED_FILES)
	cargo fmt -- --check
	cargo clippy --all-targets --all-features --release -- -D warnings

##TARGET run: build and run agent
run:
	@cargo run

##TARGET all-docker: build agent inside docker image
all-docker:
	docker run --rm --user "$(UID)":"$(GID)" -v "$(PWD)":/usr/src/myapp $(DOCKER_GIT_CRED) -w /usr/src/myapp/ $(DOCKER_IMAGE_VERSION) make

##TARGET docker: open interactive docker shell
docker:
	docker run --rm -it --user "$(UID)":"$(GID)" -v "$(PWD)":/usr/src/myapp $(DOCKER_GIT_CRED) -w /usr/src/myapp/ $(DOCKER_IMAGE_VERSION) bash

show-header:
	@printf "%s - version %s (commit %s)\n\n" "$(TARGET)" "$(VERSION)" "$(COMMIT_MSG)"

show-summary: show-header
	@printf "project:    $(PROJECT_NAME)\n"
	@printf "target:     $(TARGET)\n"
	@printf "build type: $(BUILD_TYPE)\n"
	@printf "rust:\n"
	@printf "  %s\n" "$(call get_command_version,cargo)"
	@printf "  %s\n" "$(call get_toolchain_version)"
	@printf "\n"

## help: Show help comments
help: Makefile show-summary
	@echo "Targets:"
	@sed -n 's/^##TARGET//p' $< | sort

##TARGET generate-protocols: regenerate grpc agent protocols
generate-protocols:
	libs/protocols/hack/update-generated-proto.sh all

.PHONY: \
	check \
	clean \
	default \
	docker \
	fmt \
	FORCE \
	generate-protocols \
	help \
	install \
	logging-crate-tests \
	all-docker \
	optimize \
	run \
	show-header \
	show-summary \
	test
