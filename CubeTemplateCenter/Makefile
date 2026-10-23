.PHONY: build test clean proto fmt lint

GOPATH ?= $(shell go env GOPATH)
BUILD_FLAGS := -trimpath
GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

# CubeMaster 共享代码里 image/disk.go 用了 unix.F_SETPIPE_SZ / CAP_SYS_ADMIN,
# 是 Linux-only 的。模板中心沿用它, 所以本仓库构建也固定 linux.
export GOOS := linux
export GOARCH := amd64

APPS := templatecenter

build: $(APPS)

$(APPS): %:
	@mkdir -p build
	CGO_ENABLED=0 go build -o build/$@ $(BUILD_FLAGS) \
		-ldflags "-X github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/version.GitCommit=$(GIT_COMMIT) \
		          -X github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/version.BuildTime=$(BUILD_TIME) \
		          -X github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/version.Version=$(VERSION)" \
		./cmd/$@/

# Tests use Linux-only code and gomonkey patches, so run them in the Linux
# builder with inlining disabled. These targets use CGO=0 without race detection.
test:
	CGO_ENABLED=0 go test -short -gcflags=all=-l -count=1 ./...

test-integration:
	CGO_ENABLED=0 CUBEMASTER_REQUIRE_DOCKER_TESTS=1 go test -gcflags=all=-l -count=1 ./...

fmt:
	go fmt ./...

lint:
	go fmt ./...
	golangci-lint run

clean:
	rm -rf build/
