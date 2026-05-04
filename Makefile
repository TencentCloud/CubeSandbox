# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.

BUILDER_IMAGE ?= cube-sandbox-builder:latest
BUILDER_PLATFORM ?= linux/amd64
# Set to true to use mirrors.tencent.com in the builder image (default: official Ubuntu APT).
BUILDER_APT_TENCENT_MIRROR ?= false
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

DOCKER_GIT_CRED =
ifneq ($(wildcard $(HOME)/.git-credentials),)
DOCKER_GIT_CRED += -v $(TMP_GIT_CREDENTIALS):$(BUILDER_CONTAINER_HOME)/.git-credentials
endif

.PHONY: help builder-image builder-shell builder-run prepare-builder-home prepare-tmp-git-credentials all cubemaster cubelet network-agent agent cubeapi shim manual-release web-install web-dev web-build web-preview web-lint web-api-sync web-sync-dev-env

help:
	@printf "Targets:\n"
	@printf "  builder-image  Build unified builder image (%s)\n" "$(BUILDER_IMAGE)"
	@printf "  builder-shell  Start interactive shell with persisted HOME (%s)\n" "$(BUILDER_HOME)"
	@printf "  builder-run    Run command inside builder image (BUILDER_CMD=...)\n"
	@printf "  cubemaster    Build cubemaster and cubemastercli in Docker\n"
	@printf "  cubelet       Build cubelet and cubecli in Docker\n"
	@printf "  network-agent Build network-agent in Docker\n"
	@printf "  agent         Build cube-agent in Docker\n"
	@printf "  cubeapi       Build CubeAPI (cube-api) in Docker\n"
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
	@printf "  - BUILDER_PLATFORM=%s (use linux/amd64 on Apple Silicon / arm64 Docker; unset to use host arch)\n" "$(BUILDER_PLATFORM)"
	@printf "  - BUILDER_APT_TENCENT_MIRROR=%s (set true for Tencent APT mirror; otherwise official Ubuntu)\n" "$(BUILDER_APT_TENCENT_MIRROR)"

builder-image:
	docker build $(if $(BUILDER_PLATFORM),--platform "$(BUILDER_PLATFORM)") \
		--build-arg APT_USE_TENCENT_MIRROR="$(BUILDER_APT_TENCENT_MIRROR)" \
		-t $(BUILDER_IMAGE) -f $(BUILDER_DOCKERFILE) .

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
	docker run --rm -it $(if $(BUILDER_PLATFORM),--platform "$(BUILDER_PLATFORM)") \
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
	docker run --rm -i $(if $(BUILDER_PLATFORM),--platform "$(BUILDER_PLATFORM)") \
		--user "$(UID):$(GID)" \
		-e HOME=$(BUILDER_CONTAINER_HOME) \
		-e CARGO_HOME=$(BUILDER_CONTAINER_HOME)/.cargo \
		-e RUSTUP_HOME=/usr/local/rustup \
		-e GOPATH=$(BUILDER_CONTAINER_HOME)/go \
		-e BUILDER_CMD="$(BUILDER_CMD)" \
		-v "$(ROOT_DIR)":/workspace \
		-v "$(BUILDER_HOME)":$(BUILDER_CONTAINER_HOME) \
		$(DOCKER_GIT_CRED) \
		-w /workspace \
		$(BUILDER_IMAGE) \
		bash -lc 'mkdir -p "$$HOME" "$$CARGO_HOME" "$$GOPATH" "$$HOME/.cache" "$$HOME/.config" && exec bash -lc "$$BUILDER_CMD"'

all: cubemaster cubelet network-agent

cubemaster: builder-image
	@mkdir -p "$(OUTPUT_DIR)"
	$(MAKE) builder-run BUILDER_CMD='mkdir -p /workspace/_output/bin && cd /workspace/CubeMaster && go mod download && make proto && go build -o /workspace/_output/bin/cubemaster ./cmd/cubemaster && go build -o /workspace/_output/bin/cubemastercli ./cmd/cubemastercli'

cubelet: builder-image
	@mkdir -p "$(OUTPUT_DIR)"
	$(MAKE) builder-run BUILDER_CMD='mkdir -p /workspace/_output/bin && cd /workspace/Cubelet && go mod download && make proto && go build -o /workspace/_output/bin/cubelet ./cmd/cubelet && go build -o /workspace/_output/bin/cubecli ./cmd/cubecli'

network-agent: builder-image
	@mkdir -p "$(OUTPUT_DIR)"
	$(MAKE) builder-run BUILDER_CMD='mkdir -p /workspace/_output/bin && cd /workspace/network-agent && make proto && go build -o /workspace/_output/bin/network-agent ./cmd/network-agent'

agent: builder-image
	@mkdir -p "$(OUTPUT_DIR)"
	$(MAKE) builder-run BUILDER_CMD='mkdir -p /workspace/_output/bin && cd /workspace/agent && make -j1 && install -m 0755 /workspace/agent/target/x86_64-unknown-linux-musl/release/cube-agent /workspace/_output/bin/cube-agent'

cubeapi: builder-image
	@mkdir -p "$(OUTPUT_DIR)"
	$(MAKE) builder-run BUILDER_CMD='mkdir -p /workspace/_output/bin && cd /workspace/CubeAPI && cargo build --release --locked && install -m 0755 /workspace/CubeAPI/target/release/cube-api /workspace/_output/bin/cube-api'

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
