# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

.PHONY: build run test vet fmt clean docker deps

APPS=cubeops cubeopscli
BINARY=cubeops
BIND=127.0.0.1:3010

# Version — injected at build time via ldflags.
# Priority: env var CUBE_VERSION/CUBE_COMMIT/CUBE_BUILD_TIME > local git/date fallback.
CUBE_VERSION ?= $(shell git describe --tags --abbrev=0 --match 'v*' 2>/dev/null || echo 0.0.0-dev)
CUBE_COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
CUBE_BUILD_TIME ?= $(shell date -u +'%Y-%m-%dT%H:%M:%SZ')

VERSION_PKG=github.com/tencentcloud/CubeSandbox/CubeOps/internal/version
BUILD_FLAGS=-ldflags "-s -w \
  -X '$(VERSION_PKG).Version=$(CUBE_VERSION)' \
  -X '$(VERSION_PKG).Commit=$(CUBE_COMMIT)' \
  -X '$(VERSION_PKG).BuildTime=$(CUBE_BUILD_TIME)'"

build: $(APPS)

deps:
	go mod download

$(APPS): deps
	go build $(BUILD_FLAGS) -o bin/$@ ./cmd/$@

run: build
	JWT_SECRET=test-secret-dummy \
	DATABASE_URL=mysql://cube:cube_pass@127.0.0.1:3306/cube_mvp \
	CUBE_MASTER_ADDR=http://127.0.0.1:8089 \
	./bin/$(BINARY)

test:
	go test ./... -v -count=1

vet:
	go vet ./...

fmt:
	go fmt ./...
	@command -v goimports >/dev/null 2>&1 && goimports -w . || true

clean:
	rm -rf bin/

docker:
	docker build \
		--build-arg CUBE_VERSION=$(CUBE_VERSION) \
		--build-arg CUBE_COMMIT=$(CUBE_COMMIT) \
		--build-arg CUBE_BUILD_TIME=$(CUBE_BUILD_TIME) \
		-t cube-ops:latest -f Dockerfile ..

tidy:
	go mod tidy
