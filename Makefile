# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.

BUILDER_IMAGE ?= cube-sandbox-builder:ubuntu2004
BUILDER_DOCKERFILE ?= docker/Dockerfile.builder
BUILDER_HOME ?= $(HOME)/.cache/cube-sandbox-builder
BUILDER_CONTAINER_HOME ?= /home/builder
TMP_GIT_CREDENTIALS ?= /tmp/.cube-sandbox-builder-tmp-git-credentials
BUILDER_CMD ?= bash
ROOT_DIR := $(shell pwd)
UID := $(shell id -u)
GID := $(shell id -g)
OUTPUT_DIR ?= $(ROOT_DIR)/_output/bin
RELEASE_DIR ?= $(ROOT_DIR)/_output/release
MANUAL_DEPLOY_SCRIPT ?= $(ROOT_DIR)/deploy/one-click/deploy-manual.sh
WEB_DIR ?= $(ROOT_DIR)/web
CUBECOW_DIR ?= $(ROOT_DIR)/cubecow
CUBELET_COW_THIRD_PARTY_DIR ?= $(ROOT_DIR)/Cubelet/third_party/cubecow
COW_STATICLIB ?= $(CUBELET_COW_THIRD_PARTY_DIR)/lib/libcubecow.a
COW_HEADER ?= $(CUBELET_COW_THIRD_PARTY_DIR)/include/cubecow.h

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

.PHONY: help builder-image builder-shell builder-run prepare-builder-home prepare-tmp-git-credentials all cubemaster cubelet cubecow-sdk cubecow-clean cubecow-smoke cubecow-test-native network-agent agent cubeapi cube-api shim manual-release web-install web-dev web-build web-preview web-lint web-api-sync web-sync-dev-env

help:
	@printf "Targets:\n"
	@printf "  builder-image  Build unified builder image (%s)\n" "$(BUILDER_IMAGE)"
	@printf "  builder-shell  Start interactive shell with persisted HOME (%s)\n" "$(BUILDER_HOME)"
	@printf "  builder-run    Run command inside builder image (BUILDER_CMD=...)\n"
	@printf "  cubemaster    Build cubemaster and cubemastercli in Docker\n"
	@printf "  cubelet       Build cubelet and cubecli in Docker\n"
	@printf "  cubecow-sdk   Build cubecow static library for Cubelet\n"
	@printf "  cubecow-smoke Build cubecow smoke test CLI in Docker\n"
	@printf "  cubecow-test-native Build SDK artifacts and run native tests in Docker\n"
	@printf "  network-agent Build network-agent in Docker\n"
	@printf "  agent         Build cube-agent in Docker\n"
	@printf "  cubeapi       Build CubeAPI (cube-api) in Docker\n"
	@printf "  cube-api      Alias of cubeapi\n"
	@printf "  shim          Build containerd-shim-cube-rs and cube-runtime in Docker\n"
	@printf "  all           Build cubemaster, cubelet and network-agent in Docker\n"
	@printf "  manual-release Build binaries and package manual update tarball\n"
	@printf "  web-install   Install WebUI npm dependencies\n"
	@printf "  web-dev       Start WebUI Vite dev server\n"
	@printf "  web-build     Build WebUI static assets\n"
	@printf "  web-preview   Preview built WebUI assets\n"
	@printf "  web-lint      Run WebUI lint checks\n"
	@printf "  web-api-sync  Export OpenAPI and regenerate WebUI schema types\n"
	@printf "  web-sync-dev-env Build and deploy WebUI into dev-env VM\n"
	@printf "\nNotes:\n"
	@printf "  - builder-shell forwards ~/.git-credentials when present\n"
	@printf "  - builder-run reuses the same mounted workspace and persisted HOME\n"
	@printf "  - binary outputs are written to %s\n" "$(OUTPUT_DIR)"
	@printf "  - release outputs are written to %s\n" "$(RELEASE_DIR)"
	@printf "  - Run 'make builder-image' first if image %s is missing\n" "$(BUILDER_IMAGE)"

builder-image:
	@if [ -z "$(BUILDER_FORCE_REBUILD)" ] && docker image inspect $(BUILDER_IMAGE) >/dev/null 2>&1; then \
		printf 'Builder image %s already present, skipping build (set BUILDER_FORCE_REBUILD=1 to rebuild)\n' "$(BUILDER_IMAGE)"; \
	else \
		docker build -t $(BUILDER_IMAGE) -f $(BUILDER_DOCKERFILE) ./docker; \
	fi

prepare-builder-home:
	@mkdir -p "$(BUILDER_HOME)" \
		"$(BUILDER_HOME)/.cache" \
		"$(BUILDER_HOME)/.config" \
		"$(BUILDER_HOME)/.cargo" \
		"$(BUILDER_HOME)/go"

prepare-tmp-git-credentials:
	@rm -f $(TMP_GIT_CREDENTIALS)
	@if [ -f "$(HOME)/.git-credentials" ]; then \
		cp $(HOME)/.git-credentials $(TMP_GIT_CREDENTIALS); \
		chmod 600 $(TMP_GIT_CREDENTIALS); \
	fi

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

builder-run: prepare-builder-home prepare-tmp-git-credentials
	@test -n "$(strip $(BUILDER_CMD))" || { echo "BUILDER_CMD must not be empty"; exit 1; }
	docker run --rm -i \
		--user "$(UID):$(GID)" \
		-e HOME=$(BUILDER_CONTAINER_HOME) \
		-e CARGO_HOME=$(BUILDER_CONTAINER_HOME)/.cargo \
		-e RUSTUP_HOME=/usr/local/rustup \
		-e GOPATH=$(BUILDER_CONTAINER_HOME)/go \
		-e BUILDER_CMD="$(BUILDER_CMD)" \
		-e CUBE_VERSION \
		-e CUBE_COMMIT \
		-e CUBE_BUILD_TIME \
		-v "$(ROOT_DIR)":/workspace \
		-v "$(BUILDER_HOME)":$(BUILDER_CONTAINER_HOME) \
		$(DOCKER_GIT_CRED) \
		-w /workspace \
		$(BUILDER_IMAGE) \
		bash -lc 'mkdir -p "$$HOME" "$$CARGO_HOME" "$$GOPATH" "$$HOME/.cache" "$$HOME/.config" && exec bash -lc "$$BUILDER_CMD"'

all: cubemaster cubelet network-agent

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

cubecow-clean:
	rm -rf "$(CUBELET_COW_THIRD_PARTY_DIR)"
	cd "$(CUBECOW_DIR)" && cargo clean

cubecow-smoke: builder-image
	@mkdir -p "$(OUTPUT_DIR)"
	$(MAKE) builder-run BUILDER_CMD='cd /workspace && IN_CUBE_SANDBOX_BUILDER=1 make cubecow-sdk && cd /workspace/Cubelet && go mod download && go build -a -o /workspace/_output/bin/cubecow-smoke ./pkg/cubecow/cmd/cubecow-smoke'

cubecow-test-native: builder-image
	$(MAKE) builder-run BUILDER_CMD='cd /workspace && IN_CUBE_SANDBOX_BUILDER=1 make cubecow-sdk && cd /workspace/Cubelet && go mod download && go test -a ./pkg/cubecow -run Test -count=1'

cubemaster: builder-image
	@mkdir -p "$(OUTPUT_DIR)"
	$(MAKE) builder-run BUILDER_CMD='cd /workspace/CubeMaster && make proto && make build && mkdir -p /workspace/_output/bin && cp build/cubemaster build/cubemastercli /workspace/_output/bin/'

cubelet: builder-image
	@mkdir -p "$(OUTPUT_DIR)"
	$(MAKE) builder-run BUILDER_CMD='mkdir -p /workspace/_output/bin && cd /workspace && IN_CUBE_SANDBOX_BUILDER=1 make cubecow-sdk && cd /workspace/Cubelet && go mod download && make proto && make build && cp build/cubelet build/cubecli /workspace/_output/bin/'

network-agent: builder-image
	@mkdir -p "$(OUTPUT_DIR)"
	$(MAKE) builder-run BUILDER_CMD='mkdir -p /workspace/_output/bin && cd /workspace/network-agent && make proto && make build && cp bin/network-agent /workspace/_output/bin/network-agent'

agent: builder-image
	@mkdir -p "$(OUTPUT_DIR)"
	$(MAKE) builder-run BUILDER_CMD='mkdir -p /workspace/_output/bin && cd /workspace/agent && make -j1 && install -m 0755 /workspace/agent/target/x86_64-unknown-linux-musl/release/cube-agent /workspace/_output/bin/cube-agent'

cubeapi: builder-image
	@mkdir -p "$(OUTPUT_DIR)"
	$(MAKE) builder-run BUILDER_CMD='mkdir -p /workspace/_output/bin && cd /workspace/CubeAPI && cargo build --release --locked && install -m 0755 /workspace/CubeAPI/target/release/cube-api /workspace/_output/bin/cube-api'

cube-api: cubeapi

shim: builder-image
	@mkdir -p "$(OUTPUT_DIR)"
	$(MAKE) builder-run BUILDER_CMD='mkdir -p /workspace/_output/bin && cd /workspace/CubeShim && cargo build --release --locked && install -m 0755 /workspace/CubeShim/target/release/containerd-shim-cube-rs /workspace/_output/bin/containerd-shim-cube-rs && install -m 0755 /workspace/CubeShim/target/release/cube-runtime /workspace/_output/bin/cube-runtime'

manual-release: all
	@mkdir -p "$(RELEASE_DIR)"
	@PKG_TS="$$(date +%Y%m%d-%H%M%S)"; \
	PKG_NAME="cube-manual-update-$${PKG_TS}.tar.gz"; \
	tar -C "$(OUTPUT_DIR)" -czf "$(RELEASE_DIR)/$${PKG_NAME}" cubemaster cubemastercli cubelet cubecli network-agent; \
	sha256sum "$(RELEASE_DIR)/$${PKG_NAME}" > "$(RELEASE_DIR)/$${PKG_NAME}.sha256"; \
	install -m 0755 "$(MANUAL_DEPLOY_SCRIPT)" "$(RELEASE_DIR)/deploy-manual.sh"; \
	printf 'Manual release ready:\n  %s\n  %s\n  %s\n' \
		"$(RELEASE_DIR)/$${PKG_NAME}" \
		"$(RELEASE_DIR)/$${PKG_NAME}.sha256" \
		"$(RELEASE_DIR)/deploy-manual.sh"

web-install:
	cd "$(WEB_DIR)" && npm install

web-dev:
	cd "$(WEB_DIR)" && npm run dev

web-build:
	cd "$(WEB_DIR)" && npm run build

web-preview:
	cd "$(WEB_DIR)" && npm run preview

web-lint:
	cd "$(WEB_DIR)" && npm run lint

web-api-sync:
	cd "$(WEB_DIR)" && npm run api:sync

web-sync-dev-env:
	"$(ROOT_DIR)/dev-env/internal/sync_web_to_vm.sh"
