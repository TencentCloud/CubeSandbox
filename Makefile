# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.

BUILDER_IMAGE ?= cube-sandbox-builder:ubuntu2004
BUILDER_DOCKERFILE ?= docker/Dockerfile.builder
BUILDER_HOME ?= $(HOME)/.cache/cube-sandbox-builder
BUILDER_CONTAINER_HOME ?= /home/builder
TMP_GIT_CREDENTIALS ?= /tmp/.cube-sandbox-builder-tmp-git-credentials
BUILDER_CMD ?= bash
BUILDER_RUN_EXTRA_MOUNTS ?=
# User the builder container runs as. Defaults to the host user so bind-mounted
# outputs stay host-writable; privileged test targets (e.g. cubevs-test, which
# loads eBPF / mounts bpffs) override this to 0:0 along with --privileged.
BUILDER_USER ?= $(UID):$(GID)
ROOT_DIR := $(shell pwd)
UID := $(shell id -u)
GID := $(shell id -g)
OUTPUT_DIR ?= $(ROOT_DIR)/_output/bin
RELEASE_DIR ?= $(ROOT_DIR)/_output/release
# Host path for cube-agent.ext4 (+ version). Must be under the workspace so the
# builder container can write it via the /workspace mount.
AGENT_EXT4_OUTPUT_DIR ?= $(ROOT_DIR)/_output/cube-agent
AGENT_EXT4_CONTAINER_DIR := /workspace/$(patsubst $(ROOT_DIR)/%,%,$(abspath $(AGENT_EXT4_OUTPUT_DIR)))
MANUAL_DEPLOY_SCRIPT ?= $(ROOT_DIR)/deploy/one-click/deploy-manual.sh
WEB_DIR ?= $(ROOT_DIR)/web
CUBECOW_DIR ?= $(ROOT_DIR)/cubecow
CUBES3LVOL_DIR ?= $(ROOT_DIR)/CubeS3lvol
CUBELET_COW_THIRD_PARTY_DIR ?= $(ROOT_DIR)/Cubelet/third_party/cubecow
COW_STATICLIB ?= $(CUBELET_COW_THIRD_PARTY_DIR)/lib/libcubecow.a
COW_HEADER ?= $(CUBELET_COW_THIRD_PARTY_DIR)/include/cubecow.h
TARGET_ARCH ?= $(shell uname -m | sed 's/^arm64$$/aarch64/')

# ---- Guest kernel image build ----
# `make kernel KERNEL_SRC=/path/to/linux` builds a vmlinux from the in-tree
# kernel config (configs/kernel-oc9.<arch>.config) inside the unified builder
# image.
# Supports native builds (x86_64 or aarch64) and cross builds (x86_64 <-> aarch64).
# The kernel is built out-of-tree (O=) so KERNEL_SRC is left pristine. Override
# KERNEL_TARGET_ARCH to cross-compile for an architecture other than the host;
# the matching CROSS_COMPILE prefix is selected automatically (override with
# KERNEL_CROSS_COMPILE if your toolchain uses a different prefix).
KERNEL_TARGET_ARCH ?= $(TARGET_ARCH)
KERNEL_CONFIG ?= $(ROOT_DIR)/configs/kernel-oc9.$(KERNEL_TARGET_ARCH).config
KERNEL_OUTPUT_DIR ?= $(ROOT_DIR)/_output/kernel/$(KERNEL_TARGET_ARCH)
KERNEL_IMAGE_TARGET ?= vmlinux
KERNEL_BUILD_JOBS ?=
KERNEL_CROSS_COMPILE ?=

# Top-level Rust project directories. Each owns its own Cargo workspace.
RUST_PROJECT_DIRS := \
	$(ROOT_DIR)/CubeAPI \
	$(ROOT_DIR)/CubeShim \
	$(ROOT_DIR)/agent \
	$(ROOT_DIR)/guest-init \
	$(ROOT_DIR)/cubecow \
	$(ROOT_DIR)/hypervisor

# Cargo workspaces visited by `make clean`.
RUST_CARGO_CLEAN_DIRS := \
	$(RUST_PROJECT_DIRS) \
	$(ROOT_DIR)/agent/libs

# Local Go build outputs and installed binaries. Intentionally excludes
# `_output/kernel` and `_output/release`, which are expensive to rebuild.
GO_BUILD_ARTIFACT_DIRS := \
	$(ROOT_DIR)/CubeMaster/build \
	$(ROOT_DIR)/CubeMaster/coverage \
	$(ROOT_DIR)/CubeMaster/docker/cubemaster \
	$(ROOT_DIR)/Cubelet/build \
	$(ROOT_DIR)/Cubelet/coverage \
	$(ROOT_DIR)/CubeOps/bin \
	$(ROOT_DIR)/CubeProxy/bin \
	$(ROOT_DIR)/cube-lifecycle-manager/bin \
	$(ROOT_DIR)/examples/cube-bench/bin \
	$(CUBELET_COW_THIRD_PARTY_DIR) \
	$(OUTPUT_DIR) \
	$(AGENT_EXT4_OUTPUT_DIR)

GO_BUILD_ARTIFACT_FILES := \
	$(ROOT_DIR)/cubecow/examples/go-test/cubecow-test

BINARIES := \
	agent \
	cube-init \
	cubeapi \
	cubelet \
	cubemaster \
	cubeops \
	cubevsmapdump \
	shim \
	#

# All versioned binaries should consume the canonical CUBE_VERSION /
# CUBE_COMMIT / CUBE_BUILD_TIME triplet. Keep the root Makefile's ad-hoc
# builder path aligned with the one-click release path so `_output/bin/* --version`
# is never "0.0.0-dev (unknown) built at unknown" unless the repo metadata is
# genuinely unavailable.
CUBE_VERSION ?= $(shell git describe --tags --abbrev=0 --match 'v*' 2>/dev/null || echo 0.0.0-dev)
CUBE_COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
CUBE_BUILD_TIME ?= $(shell date -u +'%Y-%m-%dT%H:%M:%SZ')
export CUBE_VERSION CUBE_COMMIT CUBE_BUILD_TIME

DOCKER_GIT_CRED =
ifneq ($(wildcard $(HOME)/.git-credentials),)
DOCKER_GIT_CRED += -v $(TMP_GIT_CREDENTIALS):$(BUILDER_CONTAINER_HOME)/.git-credentials
endif

# Builder image build-args. Set MIRROR=cn to source packages from China-reachable
# mirrors: the ubuntu apt archive (amd64 -> $(APT_MIRROR_BASE)/ubuntu, arm64 ->
# $(APT_MIRROR_BASE)/ubuntu-ports) plus the llvm.sh installer script and clang-14
# apt packages (override the LLVM mirror host with LLVM_MIRROR_BASE=...). Unset
# builds against upstream archive.ubuntu.com/ports.ubuntu.com and apt.llvm.org.
# The LLVM GPG signing key is vendored at docker/llvm-snapshot.gpg.key so the
# image build does not wget it from apt.llvm.org. This build-time MIRROR is
# unrelated to the runtime MIRROR=cn used by deploy/one-click.
APT_MIRROR_BASE ?= http://mirrors.tencent.com
LLVM_MIRROR_BASE ?= https://mirrors.zju.edu.cn/llvm-apt
BUILDER_BUILD_ARGS ?=
ifeq ($(MIRROR),cn)
BUILDER_BUILD_ARGS += --build-arg 'APT_MIRROR_BASE=$(APT_MIRROR_BASE)'
BUILDER_BUILD_ARGS += --build-arg 'LLVM_MIRROR_BASE=$(LLVM_MIRROR_BASE)'
else ifneq ($(MIRROR),)
$(warning MIRROR='$(MIRROR)' is not recognized by builder-image; expected 'cn' or empty -- building against upstream ubuntu and apt.llvm.org sources)
endif

.PHONY: all
all: $(BINARIES)

.PHONY: help
help:
	@printf "Targets:\n"
	@printf "  builder-image  Build unified builder image (%s)\n" "$(BUILDER_IMAGE)"
	@printf "  builder-shell  Start interactive shell with persisted HOME (%s)\n" "$(BUILDER_HOME)"
	@printf "  builder-run    Run command inside builder image (BUILDER_CMD=...)\n"
	@printf "  cubemaster    Build cubemaster and cubemastercli in Docker\n"
	@printf "  cubelet       Build cubelet and cubecli in Docker\n"
	@printf "  cubevsmapdump Build CubeVS eBPF business map dump tool in Docker\n"
	@printf "  cubecow-sdk   Build cubecow static library for Cubelet\n"
	@printf "  cube-s3lvol   Build CubeS3lvol (s3lvol) release in Docker\n"
	@printf "  cube-s3lvol-test Run CubeS3lvol offline integration tests in Docker\n"
	@printf "  cubecow-smoke Build cubecow smoke test CLI in Docker\n"
	@printf "  cubecow-test-native Build SDK artifacts and run native tests in Docker\n"
	@printf "  cube-proxy-sidecar Build cube-proxy-sidecar (developer-only; not in 'all')\n"
	@printf "  agent         Build cube-agent in Docker\n"
	@printf "  cube-init     Build cube-init (guest PID1) in Docker (alias: guest-init)\n"
	@printf "  guest-init    Alias for cube-init (source dir guest-init/)\n"
	@printf "  agent-ext4    Build independent cube-agent.ext4 (+ version) in Docker (alias: cube-agent-ext4)\n"
	@printf "  cube-agent-ext4 Alias for agent-ext4\n"
	@printf "  pmem-assets   Build cube-init + cube-agent.ext4 (agent-independent pmem essentials)\n"
	@printf "  cubeapi       Build CubeAPI (cube-api) in Docker\n"
	@printf "  cube-api      Alias of cubeapi\n"
	@printf "  cubeops       Build CubeOps in Docker\n"
	@printf "  cubeops-test  Run CubeOps unit tests in Docker\n"
	@printf "  shim          Build containerd-shim-cube-rs and cube-runtime in Docker\n"
	@printf "  cubemaster-test Run CubeMaster unit tests in Docker\n"
	@printf "  cubelet-test  Run Cubelet unit tests in Docker\n"
	@printf "  cube-proxy-test Run CubeProxy unit tests locally\n"
	@printf "  cube-api-test Run CubeAPI unit tests in Docker\n"
	@printf "  cubeops-test  Run CubeOps unit tests in Docker\n"
	@printf "  shim-test     Run CubeShim unit tests in Docker\n"
	@printf "  cubelog-test  Run cubelog unit tests on the host\n"
	@printf "  cubedb-test   Run CubeDB unit tests on the host\n"
	@printf "  cube-lifecycle-manager-test Run cube-lifecycle-manager unit tests in Docker\n"
	@printf "  cubelet-pkg-test Run Cubelet ./pkg/... unit tests in Docker (no coverage)\n"
	@printf "  agent-test    Run cube-agent unit tests in Docker\n"
	@printf "  hypervisor-test Run hypervisor --lib --bins unit tests in Docker\n"
	@printf "  guest-kernel  Build guest kernel vmlinux/Image (KERNEL_SRC=...; native or cross x86_64<->aarch64)\n"
	@printf "  all           Build all default binaries in Docker\n"
	@printf "  manual-release Build binaries and package manual update tarball\n"
	@printf "  clean         Remove local Go/Rust build artifacts (not global caches)\n"
	@printf "  clean-go-build-dirs Remove Go component build/bin dirs and _output/{bin,cube-agent}\n"
	@printf "  clean-rust-target-dirs cargo clean every Rust workspace in Docker\n"
	@printf "  web-install   Install WebUI npm dependencies\n"
	@printf "  web-dev       Start WebUI Vite dev server\n"
	@printf "  web-build     Build WebUI static assets\n"
	@printf "  web-preview   Preview built WebUI assets\n"
	@printf "  web-lint      Run WebUI lint checks\n"
	@printf "  web-fmt       Format WebUI sources\n"
	@printf "  fmt           Format all component directories\n"
	@printf "  web-api-sync  Export OpenAPI and regenerate WebUI schema types\n"
	@printf "  web-sync-dev-env Build and deploy WebUI into dev-env VM\n"
	@printf "\nNotes:\n"
	@printf "  - builder-shell forwards ~/.git-credentials when present\n"
	@printf "  - builder-run reuses the same mounted workspace and persisted HOME\n"
	@printf "  - binary outputs are written to %s\n" "$(OUTPUT_DIR)"
	@printf "  - cube-agent.ext4 outputs are written to %s\n" "$(AGENT_EXT4_OUTPUT_DIR)"
	@printf "  - release outputs are written to %s\n" "$(RELEASE_DIR)"
	@printf "  - Run 'make builder-image' first if image %s is missing\n" "$(BUILDER_IMAGE)"

.PHONY: builder-image
# Context is the repo root (Dockerfile COPYs CubeS3lvol/setup_dep.sh + patches/).
# Rebuilds when the image's s3lvol SPDK/AWS stamps no longer match the current
# pin + patches (toolchain layers stay cached). BUILDER_FORCE_REBUILD=1 forces it.
builder-image:
	@expected_spdk="$$($(CUBES3LVOL_DIR)/setup_dep.sh --print-stamp spdk)"; \
	expected_aws="$$($(CUBES3LVOL_DIR)/setup_dep.sh --print-stamp aws)"; \
	need_build=0; \
	if [ -n "$(BUILDER_FORCE_REBUILD)" ]; then \
		need_build=1; \
	elif ! docker image inspect $(BUILDER_IMAGE) >/dev/null 2>&1; then \
		need_build=1; \
	else \
		actual_spdk="$$(docker image inspect -f '{{index .Config.Labels "org.cubesandbox.s3lvol.spdk-stamp"}}' $(BUILDER_IMAGE) 2>/dev/null || true)"; \
		actual_aws="$$(docker image inspect -f '{{index .Config.Labels "org.cubesandbox.s3lvol.aws-stamp"}}' $(BUILDER_IMAGE) 2>/dev/null || true)"; \
		if [ "$$actual_spdk" != "$$expected_spdk" ] || [ "$$actual_aws" != "$$expected_aws" ]; then \
			printf 'Builder image %s s3lvol stamps stale (spdk %s -> %s, aws %s -> %s), rebuilding\n' \
				"$(BUILDER_IMAGE)" "$${actual_spdk:-none}" "$$expected_spdk" "$${actual_aws:-none}" "$$expected_aws"; \
			need_build=1; \
		fi; \
	fi; \
	if [ "$$need_build" -eq 0 ]; then \
		printf 'Builder image %s already present and s3lvol stamps match, skipping build (set BUILDER_FORCE_REBUILD=1 to rebuild)\n' "$(BUILDER_IMAGE)"; \
	else \
		docker build $(if $(filter-out 0,$(BUILDER_FORCE_REBUILD)),--no-cache) $(BUILDER_BUILD_ARGS) \
			--build-arg S3LVOL_SPDK_STAMP="$$expected_spdk" \
			--build-arg S3LVOL_AWS_STAMP="$$expected_aws" \
			-t $(BUILDER_IMAGE) -f $(BUILDER_DOCKERFILE) .; \
	fi

.PHONY: prepare-builder-home
prepare-builder-home:
	@mkdir -p "$(BUILDER_HOME)" \
		"$(BUILDER_HOME)/.cache" \
		"$(BUILDER_HOME)/.config" \
		"$(BUILDER_HOME)/.cargo" \
		"$(BUILDER_HOME)/go"

.PHONY: prepare-tmp-git-credentials
prepare-tmp-git-credentials:
	@rm -f $(TMP_GIT_CREDENTIALS)
	@if [ -f "$(HOME)/.git-credentials" ]; then \
		cp $(HOME)/.git-credentials $(TMP_GIT_CREDENTIALS); \
		chmod 600 $(TMP_GIT_CREDENTIALS); \
	fi

.PHONY: builder-shell
builder-shell: prepare-builder-home prepare-tmp-git-credentials
	docker run --rm -it \
		--user "$(UID):$(GID)" \
		-e HOME=$(BUILDER_CONTAINER_HOME) \
		-e CARGO_HOME=$(BUILDER_CONTAINER_HOME)/.cargo \
		-e RUSTUP_HOME=/usr/local/rustup \
		-e GOPATH=$(BUILDER_CONTAINER_HOME)/go \
		-v "$(ROOT_DIR)":/workspace \
		-v "$(BUILDER_HOME)":$(BUILDER_CONTAINER_HOME) \
		$(DOCKER_GIT_CRED) \
		-w /workspace \
		$(BUILDER_IMAGE) \
		bash -lc 'mkdir -p "$$HOME" "$$CARGO_HOME" "$$GOPATH" "$$HOME/.cache" "$$HOME/.config" && exec bash'

.PHONY: builder-run
builder-run: prepare-builder-home prepare-tmp-git-credentials
ifeq ($(strip $(BUILDER_CMD)),)
	$(error BUILDER_CMD must not be empty)
endif
# The container can run as a different user than the owner of files under the
# mounted workspace. In CI, cube-s3lvol-test runs as root (0:0, DPDK EAL), but
# deps/spdk restored from the actions/cache are owned by the runner uid, so
# git inside the container refuses them ("dubious ownership"). Mark every repo
# under the mounted workspace as safe -- this only relaxes that check inside
# the throwaway container.
	docker run --rm -i \
		--user "$(BUILDER_USER)" \
		-e HOME=$(BUILDER_CONTAINER_HOME) \
		-e CARGO_HOME=$(BUILDER_CONTAINER_HOME)/.cargo \
		-e RUSTUP_HOME=/usr/local/rustup \
		-e GOPATH=$(BUILDER_CONTAINER_HOME)/go \
		-e BUILDER_CMD="$(BUILDER_CMD)" \
		-e CUBE_VERSION \
		-e CUBE_COMMIT \
		-e CUBE_BUILD_TIME \
		-e ONE_CLICK_BUILD_JOBS \
		-v "$(ROOT_DIR)":/workspace \
		-v "$(BUILDER_HOME)":$(BUILDER_CONTAINER_HOME) \
		$(BUILDER_RUN_EXTRA_MOUNTS) \
		$(DOCKER_GIT_CRED) \
		-w /workspace \
		$(BUILDER_IMAGE) \
		bash -lc 'mkdir -p "$$HOME" "$$CARGO_HOME" "$$GOPATH" "$$HOME/.cache" "$$HOME/.config" && git config --global --add safe.directory "*" && exec bash -lc "$$BUILDER_CMD"'

.PHONY: cubecow-sdk
cubecow-sdk:
ifeq ($(IN_CUBE_SANDBOX_BUILDER),1)
	@mkdir -p "$(CUBELET_COW_THIRD_PARTY_DIR)/lib" "$(CUBELET_COW_THIRD_PARTY_DIR)/include"
	cd "$(CUBECOW_DIR)" && cargo build --release -p cubecow
	install -m 0644 "$(CUBECOW_DIR)/target/release/libcubecow.a" "$(COW_STATICLIB)"
	install -m 0644 "$(CUBECOW_DIR)/include/cubecow.h" "$(COW_HEADER)"
else
	$(MAKE) builder-image
	$(MAKE) builder-run BUILDER_CMD='cd /workspace && IN_CUBE_SANDBOX_BUILDER=1 make cubecow-sdk'
endif

# cube-s3lvol: build the CubeS3lvol release in Docker, mirroring track_s3lvol.
# setup_dep.sh reuses /opt/s3lvol-* from the builder when stamps match.
.PHONY: cube-s3lvol
cube-s3lvol:
ifeq ($(IN_CUBE_SANDBOX_BUILDER),1)
	@mkdir -p "$(OUTPUT_DIR)/CubeS3lvol"
	cd "$(CUBES3LVOL_DIR)" && AWS_BUILD_TYPE=RelWithDebInfo ./setup_dep.sh --jobs "$$(nproc)"
	cd "$(CUBES3LVOL_DIR)" && AWS_BUILD_TYPE=RelWithDebInfo make S3LVOL_BUILD_TYPE=release -j"$$(nproc)"
	cd "$(CUBES3LVOL_DIR)" && AWS_BUILD_TYPE=RelWithDebInfo ./make_release.sh --no-tar --skip-smoke --version "$(CUBE_VERSION)" --outdir "$(OUTPUT_DIR)/CubeS3lvol"
	@printf 'CubeS3lvol release: %s/CubeS3lvol/s3lvol-*/ (bin/s3lvol_tgt + scripts/ + VERSION)\n' "$(OUTPUT_DIR)"
else
	$(MAKE) builder-image
	$(MAKE) builder-run BUILDER_CMD='cd /workspace && IN_CUBE_SANDBOX_BUILDER=1 make cube-s3lvol'
endif

# cube-s3lvol-test: the CI gate (check-rules + debug make + check-offline).
# Journal/wal/cache/local_dev tests write aio files under /data; tmpfs keeps
# that off the host. The DPDK EAL tests (--no-huge) still need locked memory,
# /sys cgroup/cpu topology, and a writable runtime dir; a non-root docker
# process cannot initialise the EAL (spdk_env_init -> exit 77). Same privileged
# root pattern as cubevs-test. Incremental setup_dep.sh / make do not rewrite
# existing 1000-owned deps. setup_dep.sh is incremental.
.PHONY: cube-s3lvol-test
cube-s3lvol-test:
ifeq ($(IN_CUBE_SANDBOX_BUILDER),1)
	cd "$(CUBES3LVOL_DIR)" && ./setup_dep.sh --jobs "$$(nproc)"
	cd "$(CUBES3LVOL_DIR)" && make check-rules
	cd "$(CUBES3LVOL_DIR)" && make -j"$$(nproc)"
	cd "$(CUBES3LVOL_DIR)" && mkdir -p /tmp/s3lvol-dpdk && \
		RTE_RUNTIME_DIR=/tmp/s3lvol-dpdk make check-offline
else
	$(MAKE) builder-image
	$(MAKE) builder-run \
		BUILDER_USER=0:0 \
		BUILDER_RUN_EXTRA_MOUNTS='--privileged --tmpfs /data:rw,mode=1777 --ulimit memlock=-1' \
		BUILDER_CMD='cd /workspace && IN_CUBE_SANDBOX_BUILDER=1 make cube-s3lvol-test'
endif

.PHONY: cubecow-clean
cubecow-clean:
	rm -rf "$(CUBELET_COW_THIRD_PARTY_DIR)"
	cd "$(CUBECOW_DIR)" && cargo clean

.PHONY: clean-go-build-dirs
clean-go-build-dirs:
	@for dir in $(GO_BUILD_ARTIFACT_DIRS); do \
		if [ -d "$$dir" ]; then \
			printf '  %-8s %s\n' "RM" "$$dir"; \
			rm -rf "$$dir"; \
		fi; \
	done
	@for f in $(GO_BUILD_ARTIFACT_FILES); do \
		if [ -e "$$f" ]; then \
			printf '  %-8s %s\n' "RM" "$$f"; \
			rm -f "$$f"; \
		fi; \
	done

.PHONY: clean-rust-target-dirs
clean-rust-target-dirs:
ifeq ($(IN_CUBE_SANDBOX_BUILDER),1)
	@for dir in $(RUST_CARGO_CLEAN_DIRS); do \
		printf '  %-8s %s\n' "CARGO" "clean $$dir"; \
		cargo clean --manifest-path "$$dir/Cargo.toml"; \
	done
else
	$(MAKE) builder-image
	$(MAKE) builder-run BUILDER_CMD='cd /workspace && IN_CUBE_SANDBOX_BUILDER=1 make clean-rust-target-dirs'
endif

.PHONY: clean
clean: clean-go-build-dirs clean-rust-target-dirs

.PHONY: cubecow-smoke
cubecow-smoke: builder-image
	@mkdir -p "$(OUTPUT_DIR)"
	$(MAKE) builder-run BUILDER_CMD='cd /workspace && IN_CUBE_SANDBOX_BUILDER=1 make cubecow-sdk && cd /workspace/Cubelet && go mod download && go build -a -o /workspace/_output/bin/cubecow-smoke ./pkg/cubecow/cmd/cubecow-smoke'

.PHONY: cubecow-test-native
cubecow-test-native: builder-image
	$(MAKE) builder-run BUILDER_CMD='cd /workspace && IN_CUBE_SANDBOX_BUILDER=1 make cubecow-sdk && cd /workspace/Cubelet && go mod download && go test -a ./pkg/cubecow -run Test -count=1'

.PHONY: cubemaster
cubemaster: builder-image
	@mkdir -p "$(OUTPUT_DIR)"
	$(MAKE) builder-run BUILDER_CMD='cd /workspace/CubeMaster && make proto && CGO_ENABLED=0 make build && mkdir -p /workspace/_output/bin && cp build/cubemaster build/cubemastercli /workspace/_output/bin/'

.PHONY: cubelet
cubelet: builder-image
	@mkdir -p "$(OUTPUT_DIR)"
	# Cubelet embeds the network runtime and links CubeNet/cubevs; bpf2go outputs
	# are gitignored, so generate them before compiling cubelet.
	$(MAKE) builder-run BUILDER_CMD='mkdir -p /workspace/_output/bin && cd /workspace && IN_CUBE_SANDBOX_BUILDER=1 make cubecow-sdk && cd /workspace/CubeNet/cubevs && make gen && cd /workspace/Cubelet && go mod download && make proto && make build && cp build/cubelet build/cubecli /workspace/_output/bin/'

.PHONY: cubevsmapdump
cubevsmapdump: builder-image
	@mkdir -p "$(OUTPUT_DIR)"
	$(MAKE) builder-run BUILDER_CMD='mkdir -p /workspace/_output/bin && cd /workspace/CubeNet/cubevs && make gen && go build -o /workspace/_output/bin/cubevsmapdump ./cmd/cubevsmapdump'

.PHONY: cube-proxy-sidecar
cube-proxy-sidecar: builder-image
	@mkdir -p "$(OUTPUT_DIR)"
	$(MAKE) builder-run BUILDER_CMD="mkdir -p /workspace/_output/bin && cd /workspace/CubeProxy/sidecar && go mod download && CGO_ENABLED=0 GOOS=linux GOARCH=$$(go env GOARCH) go build -trimpath -tags 'netgo osusergo' -ldflags '-s -w' -o /workspace/_output/bin/cube-proxy-sidecar ./cmd/sidecar"

.PHONY: agent
agent: builder-image
	@mkdir -p "$(OUTPUT_DIR)"
	$(MAKE) builder-run BUILDER_CMD='mkdir -p /workspace/_output/bin && cd /workspace/agent && make -j1 &&  make BINDIR=/workspace/_output/bin install'

.PHONY: cube-init guest-init
cube-init guest-init: builder-image
	@mkdir -p "$(OUTPUT_DIR)"
	$(MAKE) builder-run BUILDER_CMD='mkdir -p /workspace/_output/bin && cd /workspace/guest-init && make -j1 && make BINDIR=/workspace/_output/bin install'

# Independent cube-agent.ext4 plane file for virtio-pmem1 (agent-independent pmem).
# Builds the musl-static cube-agent inside the builder, then packages
# cube-agent.ext4 + version via deploy/one-click/build-agent-ext4.sh.
# Override output with: make agent-ext4 AGENT_EXT4_OUTPUT_DIR=$$PWD/_output/cube-agent
# (path must stay under the repo so the builder /workspace mount can write it)
# Reuse a prebuilt binary already under the workspace:
#   ONE_CLICK_CUBE_AGENT_BIN=/workspace/_output/bin/cube-agent make agent-ext4
.PHONY: agent-ext4 cube-agent-ext4
agent-ext4 cube-agent-ext4: builder-image
	@case "$(abspath $(AGENT_EXT4_OUTPUT_DIR))" in \
		"$(ROOT_DIR)"|"$(ROOT_DIR)"/*) ;; \
		*) echo "ERROR: AGENT_EXT4_OUTPUT_DIR must be under $(ROOT_DIR) (got $(AGENT_EXT4_OUTPUT_DIR))"; exit 1 ;; \
	esac
	@mkdir -p "$(AGENT_EXT4_OUTPUT_DIR)"
	$(MAKE) builder-run BUILDER_CMD='mkdir -p $(AGENT_EXT4_CONTAINER_DIR) && OUTPUT_DIR=$(AGENT_EXT4_CONTAINER_DIR) $(if $(strip $(ONE_CLICK_CUBE_AGENT_BIN)),ONE_CLICK_CUBE_AGENT_BIN=$(ONE_CLICK_CUBE_AGENT_BIN) )bash /workspace/deploy/one-click/build-agent-ext4.sh'

# Essentials for agent-independent pmem: guest PID1 binary + agent plane file.
# Does not build the full guest OS image or shim/kernel (use one-click for that).
.PHONY: pmem-assets
pmem-assets: cube-init agent-ext4

.PHONY: cubeapi
cubeapi: builder-image
	@mkdir -p "$(OUTPUT_DIR)"
	$(MAKE) builder-run BUILDER_CMD='mkdir -p /workspace/_output/bin && cd /workspace/CubeAPI && CC_$(TARGET_ARCH)_unknown_linux_musl=musl-gcc cargo build --release --locked --target $(TARGET_ARCH)-unknown-linux-musl && install -m 0755 /workspace/CubeAPI/target/$(TARGET_ARCH)-unknown-linux-musl/release/cube-api /workspace/_output/bin/cube-api'

.PHONY: cube-api
cube-api: cubeapi

.PHONY: cubeops
cubeops: builder-image
	@mkdir -p "$(OUTPUT_DIR)"
	$(MAKE) builder-run BUILDER_CMD="mkdir -p /workspace/_output/bin && cd /workspace/CubeOps && go mod download && CGO_ENABLED=0 GOOS=linux GOARCH=$$(go env GOARCH) go build -ldflags '-s -w -X github.com/tencentcloud/CubeSandbox/CubeOps/internal/version.Version=$(CUBE_VERSION) -X github.com/tencentcloud/CubeSandbox/CubeOps/internal/version.Commit=$(CUBE_COMMIT) -X github.com/tencentcloud/CubeSandbox/CubeOps/internal/version.BuildTime=$(CUBE_BUILD_TIME)' -o /workspace/_output/bin/cubeops ./cmd/cubeops && go build -ldflags '-s -w' -o /workspace/_output/bin/cubeopscli ./cmd/cubeopscli"

.PHONY: cubeops-test
cubeops-test: builder-image
	$(MAKE) builder-run BUILDER_CMD='cd /workspace/CubeOps && go mod download && go test ./...'

.PHONY: cubemaster-test
cubemaster-test: builder-image
	$(MAKE) builder-run BUILDER_CMD='cd /workspace/CubeMaster && go mod download && make test'

.PHONY: cubelet-test
cubelet-test: builder-image
	$(MAKE) builder-run BUILDER_CMD='cd /workspace && IN_CUBE_SANDBOX_BUILDER=1 make cubecow-sdk && cd /workspace/CubeNet/cubevs && make gen && cd /workspace/Cubelet && go mod download && make test'

.PHONY: cube-proxy-test
cube-proxy-test:
	$(MAKE) -C CubeProxy test

.PHONY: cube-api-test
cube-api-test: builder-image
	$(MAKE) builder-run BUILDER_CMD='cd /workspace/CubeAPI && make test'

.PHONY: shim-test
shim-test: builder-image
	$(MAKE) builder-run BUILDER_CMD='cd /workspace/CubeShim && make test'

# cubelog/cubedb run on the host: both are pure Go with no CGO and no
# builder-only deps, so the host toolchain is sufficient and skipping the
# container is faster.
.PHONY: cubelog-test
cubelog-test:
	cd cubelog && go test -short ./...

.PHONY: cubedb-test
cubedb-test:
	cd CubeDB && go mod download && go test ./...

.PHONY: cube-lifecycle-manager-test
cube-lifecycle-manager-test: builder-image
	$(MAKE) builder-run BUILDER_CMD='cd /workspace/cube-lifecycle-manager && go mod download && go test ./...'

# cubelet-pkg-test bypasses cubelet-test: that target runs `go test
# -coverprofile`, and the builder's Go toolchain lacks the `covdata` tool, so
# any coverage build fails. Run only ./pkg/... with -short (skips the
# Redis/KVM-dependent cases), which is self-contained in the builder.
.PHONY: cubelet-pkg-test
cubelet-pkg-test: builder-image
	$(MAKE) builder-run BUILDER_CMD='cd /workspace && IN_CUBE_SANDBOX_BUILDER=1 make cubecow-sdk && cd /workspace/Cubelet && go mod download && make proto && go test -short ./pkg/...'

# cubevs-test runs the CubeNet/cubevs module's own unit tests (dataplane policy,
# DNS learning, migration, dump, classify), which the cubelet targets never
# compile. It regenerates the BPF objects first (make gen) since the test files
# embed them, then runs the full module test set. The eBPF-loading tests need a
# privileged root container (CAP_BPF/CAP_SYS_ADMIN for bpf() and the bpffs
# mount), so this runs the builder privileged as root. Note: the generated .o
# files under CubeNet/cubevs become root-owned in the bind-mounted workspace;
# that is harmless in CI (fresh checkout) but may require sudo to clean locally.
# Use plain `go test ./...` (no -coverprofile): the builder lacks covdata.
.PHONY: cubevs-test
cubevs-test: builder-image
	$(MAKE) builder-run BUILDER_USER=0:0 BUILDER_RUN_EXTRA_MOUNTS='--privileged' BUILDER_CMD='cd /workspace/CubeNet/cubevs && make gen && go test ./...'

.PHONY: agent-test
agent-test: builder-image
	$(MAKE) builder-run BUILDER_CMD='cd /workspace/agent && make test'

# Only unit tests (--lib --bins) run here; the tests/integration.rs target
# needs a full VM. This does not pass /dev/kvm into the builder, so the
# runtime-KVM vmm tests are not reached (see tests/unittest/run.sh
# hypervisor-kvm for those).
.PHONY: hypervisor-test
hypervisor-test: builder-image
	$(MAKE) builder-run BUILDER_CMD='cd /workspace/hypervisor && cargo test --features kvm --lib --bins'

.PHONY: shim
shim: builder-image
	@mkdir -p "$(OUTPUT_DIR)"
	$(MAKE) builder-run BUILDER_CMD='mkdir -p /workspace/_output/bin && cd /workspace/CubeShim && cargo build --release --locked && install -m 0755 /workspace/CubeShim/target/release/containerd-shim-cube-rs /workspace/_output/bin/containerd-shim-cube-rs && install -m 0755 /workspace/CubeShim/target/release/cube-runtime /workspace/_output/bin/cube-runtime'

# Build a guest kernel image (vmlinux for x86_64, Image for aarch64) from an external kernel source tree.
#   make guest-kernel KERNEL_SRC=/path/to/linux                            # native build for the host arch
#   make guest-kernel KERNEL_SRC=/path/to/linux KERNEL_TARGET_ARCH=aarch64 # cross build
# KERNEL_SRC is mounted into the builder at /kernel-src; the config is taken
# from configs/kernel-oc9.$(KERNEL_TARGET_ARCH).config and the resulting Linux kernel image
# is written to $(KERNEL_OUTPUT_DIR) on the host with name as vmlinux.
.PHONY: guest-kernel
guest-kernel: kernel-precheck builder-image
	@mkdir -p "$(KERNEL_OUTPUT_DIR)"
	$(MAKE) builder-run \
		BUILDER_RUN_EXTRA_MOUNTS='-v $(abspath $(KERNEL_SRC)):/kernel-src' \
		BUILDER_CMD='KERNEL_SRC_DIR=/kernel-src KERNEL_TARGET_ARCH=$(KERNEL_TARGET_ARCH) KERNEL_CONFIG=/workspace/configs/kernel-oc9.$(KERNEL_TARGET_ARCH).config KERNEL_OUTPUT_DIR=/workspace/_output/kernel/$(KERNEL_TARGET_ARCH) KERNEL_CROSS_COMPILE=$(strip $(KERNEL_CROSS_COMPILE)) KERNEL_BUILD_JOBS=$(strip $(KERNEL_BUILD_JOBS)) bash /workspace/scripts/build-kernel.sh'

.PHONY: kernel-precheck
kernel-precheck:
	@test -n "$(strip $(KERNEL_SRC))" || { echo "ERROR: KERNEL_SRC must point to a Linux kernel source tree (e.g. make guest-kernel KERNEL_SRC=/path/to/linux)"; exit 1; }
	@test -d "$(KERNEL_SRC)" || { echo "ERROR: KERNEL_SRC '$(KERNEL_SRC)' is not a directory"; exit 1; }
	@test -f "$(KERNEL_CONFIG)" || { echo "ERROR: kernel config not found: $(KERNEL_CONFIG)"; exit 1; }

.PHONY: manual-release
manual-release: all
	@mkdir -p "$(RELEASE_DIR)"
	@PKG_TS="$$(date +%Y%m%d-%H%M%S)"; \
	PKG_NAME="cube-manual-update-$${PKG_TS}.tar.gz"; \
	tar -C "$(OUTPUT_DIR)" -czf "$(RELEASE_DIR)/$${PKG_NAME}" cubemaster cubemastercli cubelet cubecli cubevsmapdump; \
	sha256sum "$(RELEASE_DIR)/$${PKG_NAME}" > "$(RELEASE_DIR)/$${PKG_NAME}.sha256"; \
	install -m 0755 "$(MANUAL_DEPLOY_SCRIPT)" "$(RELEASE_DIR)/deploy-manual.sh"; \
	printf 'Manual release ready:\n  %s\n  %s\n  %s\n' \
		"$(RELEASE_DIR)/$${PKG_NAME}" \
		"$(RELEASE_DIR)/$${PKG_NAME}.sha256" \
		"$(RELEASE_DIR)/deploy-manual.sh"

.PHONY: web-install
web-install:
	cd "$(WEB_DIR)" && npm install

.PHONY: web-dev
web-dev:
	cd "$(WEB_DIR)" && npm run dev

.PHONY: web-build
web-build:
	cd "$(WEB_DIR)" && npm run build

.PHONY: web-preview
web-preview:
	cd "$(WEB_DIR)" && npm run preview

.PHONY: web-lint
web-lint:
	cd "$(WEB_DIR)" && npm run lint

.PHONY: web-fmt
web-fmt:
	@$(MAKE) -C web fmt

.PHONY: web-api-sync
web-api-sync:
	cd "$(WEB_DIR)" && npm run api:sync

.PHONY: web-sync-dev-env
web-sync-dev-env:
	"$(ROOT_DIR)/dev-env/internal/sync_web_to_vm.sh"

# Run make fmt in each component directory that has a fmt target.
# Components without formattable code (e.g. CubeProxy) are skipped.
# Outside the builder, Go/Rust fmt routes through builder-run so
# agent/Makefile's MUSL+SECCOMP parse-time libseccomp.a check does not
# fail on the host. Web fmt stays on the host: the builder image has no
# npm (same split as fmt-check.yml).
.PHONY: fmt
fmt:
ifeq ($(IN_CUBE_SANDBOX_BUILDER),1)
	@printf '  %-8s %s\n' "FMT" "agent"
	@$(MAKE) -C agent fmt
	@printf '  %-8s %s\n' "FMT" "guest-init"
	@$(MAKE) -C guest-init fmt
	@printf '  %-8s %s\n' "FMT" "cubecow"
	@$(MAKE) -C cubecow fmt
	@printf '  %-8s %s\n' "FMT" "CubeAPI"
	@$(MAKE) -C CubeAPI fmt
	@printf '  %-8s %s\n' "FMT" "Cubelet"
	@$(MAKE) -C Cubelet fmt
	@printf '  %-8s %s\n' "FMT" "cubelog"
	@$(MAKE) -C cubelog fmt
	@printf '  %-8s %s\n' "FMT" "CubeMaster"
	@$(MAKE) -C CubeMaster fmt
	@printf '  %-8s %s\n' "FMT" "CubeNet"
	@$(MAKE) -C CubeNet fmt
	@printf '  %-8s %s\n' "FMT" "CubeOps"
	@$(MAKE) -C CubeOps fmt
	@printf '  %-8s %s\n' "FMT" "CubeShim"
	@$(MAKE) -C CubeShim fmt
	@printf '  %-8s %s\n' "FMT" "cube-lifecycle-manager"
	@$(MAKE) -C cube-lifecycle-manager fmt
	@printf '  %-8s %s\n' "FMT" "hypervisor"
	@$(MAKE) -C hypervisor fmt
	@printf '  %-8s %s\n' "FMT" "sdk/go"
	@$(MAKE) -C sdk/go fmt
	@printf '  %-8s %s\n' "FMT" "examples/cube-bench"
	@$(MAKE) -C examples/cube-bench fmt
else
	@$(MAKE) builder-image
	$(MAKE) builder-run BUILDER_CMD='cd /workspace && IN_CUBE_SANDBOX_BUILDER=1 make fmt'
	@printf '  %-8s %s\n' "FMT" "web"
	@if command -v npm >/dev/null 2>&1; then \
		$(MAKE) -C web fmt; \
	else \
		printf '  %-8s %s\n' "SKIP" "web (npm not available on host)"; \
	fi
endif
