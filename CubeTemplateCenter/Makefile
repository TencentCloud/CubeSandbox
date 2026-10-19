.PHONY: build test test-docker test-integration clean proto fmt lint

GOPATH ?= $(shell go env GOPATH)
BUILD_FLAGS := -trimpath
GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

# CubeMaster 共享代码里 image/disk.go 用了 unix.F_SETPIPE_SZ / CAP_SYS_ADMIN,
# 是 Linux-only 的。模板中心沿用它, 所以本仓库构建也固定 linux.
export GOOS := linux
export GOARCH ?= $(shell go env GOARCH)

APPS := templatecenter

build: $(APPS)

$(APPS): %:
	@mkdir -p build
	CGO_ENABLED=0 go build -o build/$@ $(BUILD_FLAGS) \
		-ldflags "-X github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/version.GitCommit=$(GIT_COMMIT) \
		          -X github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/version.BuildTime=$(BUILD_TIME) \
		          -X github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/version.Version=$(VERSION)" \
		./cmd/$@/

# -gcflags=all=-l disables inlining: the gomonkey-based tests in ./pkg/image
# patch function machine code, and an inlined call site bypasses the patch.
# The Docker-backed MySQL tests are excluded here with -short; they run in the
# separate test-docker target on a host that can reach a Docker daemon.
test:
	CGO_ENABLED=0 go test -short -gcflags=all=-l -count=1 ./...

# Docker-backed tests must run where the daemon and its published 127.0.0.1
# ports are reachable. Failing instead of skipping keeps CI honest: a missing
# daemon is an environment error, not a reason to report green.
test-docker:
	@command -v docker >/dev/null 2>&1 || { echo "error: docker is not installed"; exit 1; }
	@docker info >/dev/null 2>&1 || { echo "error: docker daemon is not reachable"; exit 1; }
	CI=true CUBEMASTER_REQUIRE_DOCKER_TESTS=1 CGO_ENABLED=0 go test -v -count=1 \
		-gcflags=all=-l -timeout=20m ./pkg/reconcile

# Full local integration run; CI uses the narrower test-docker lane.
test-integration:
	CGO_ENABLED=0 CUBEMASTER_REQUIRE_DOCKER_TESTS=1 go test -gcflags=all=-l -count=1 ./...

fmt:
	go fmt ./...

lint:
	go fmt ./...
	golangci-lint run

clean:
	rm -rf build/
