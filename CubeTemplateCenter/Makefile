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

# 本仓库无业务测试, test target 仅编译验证 (CGO=0 + GOOS=linux 跨编译).
# 在 mac 上 GOOS=linux + CGO_ENABLED=1 会因为缺 Linux header 失败,
# 所以 CGO_ENABLED=0 (不启用 race). CI 里跑测试请用 Linux 容器.
test:
	CGO_ENABLED=0 go test -short -count=1 ./...

test-integration:
	CGO_ENABLED=0 CUBEMASTER_REQUIRE_DOCKER_TESTS=1 go test -count=1 ./...

fmt:
	go fmt ./...

lint:
	go fmt ./...
	golangci-lint run

clean:
	rm -rf build/
