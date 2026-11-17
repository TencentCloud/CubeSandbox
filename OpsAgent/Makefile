# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

.PHONY: fmt test build docker

# Version — injected at build time via ldflags.
# Priority: env var CUBE_VERSION/CUBE_COMMIT/CUBE_BUILD_TIME > local git/date fallback.
CUBE_VERSION ?= $(shell git describe --tags --abbrev=0 --match 'v*' 2>/dev/null || echo 0.0.0-dev)
CUBE_COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
CUBE_BUILD_TIME ?= $(shell date -u +'%Y-%m-%dT%H:%M:%SZ')

VERSION_PKG=github.com/tencentcloud/CubeSandbox/OpsAgent/internal/version
BUILD_FLAGS=-ldflags "-s -w \
  -X '$(VERSION_PKG).Version=$(CUBE_VERSION)' \
  -X '$(VERSION_PKG).Commit=$(CUBE_COMMIT)' \
  -X '$(VERSION_PKG).BuildTime=$(CUBE_BUILD_TIME)'"

fmt:
	go fmt ./...

test:
	go test ./...

build:
	go build $(BUILD_FLAGS) -o bin/ops-agent ./cmd/ops-agent

# Build context is the repository root (sibling modules pkgs/CubeLog are needed).
docker:
	docker build \
		--build-arg CUBE_VERSION=$(CUBE_VERSION) \
		--build-arg CUBE_COMMIT=$(CUBE_COMMIT) \
		--build-arg CUBE_BUILD_TIME=$(CUBE_BUILD_TIME) \
		-t cube-ops-agent:latest -f Dockerfile ..
