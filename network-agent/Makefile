ifeq ($(V),1)
Q =
msg =
else
Q = @
msg = @printf '  %-8s %s%s\n' "$(1)" "$(notdir $(2))" "$(if $(3), $(3))";
MAKEFLAGS += --no-print-directory
endif

ROOT_DIR := $(shell pwd)
BUILD_DIR := $(ROOT_DIR)/bin
PROTO_SRC := api/v1/network_agent.proto
CLIENT_PROTO_DIR := ../Cubelet/pkg/networkagentclient/pb
CLIENT_PROTO := $(CLIENT_PROTO_DIR)/network_agent.proto
APP := network-agent
GO_BIN_DIR := $(shell if [ -n "$$(go env GOBIN)" ]; then printf "%s" "$$(go env GOBIN)"; else printf "%s/bin" "$$(go env GOPATH)"; fi)

# Version — injected at build time via ldflags.
# Priority: env var CUBE_VERSION/CUBE_COMMIT/CUBE_BUILD_TIME > local git/date fallback.
CUBE_VERSION ?= $(shell git describe --tags --abbrev=0 --match 'v*' 2>/dev/null || echo 0.0.0-dev)
CUBE_COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
CUBE_BUILD_TIME ?= $(shell date -u +'%Y-%m-%dT%H:%M:%SZ')

VERSION_PKG := github.com/tencentcloud/CubeSandbox/network-agent/pkg/version
LDFLAGS := -ldflags "-s -w \
  -X '$(VERSION_PKG).Version=$(CUBE_VERSION)' \
  -X '$(VERSION_PKG).Commit=$(CUBE_COMMIT)' \
  -X '$(VERSION_PKG).BuildTime=$(CUBE_BUILD_TIME)'"

export PATH := $(GO_BIN_DIR):$(PATH)

.PHONY: build fmt proto test clean check-proto-tools sync-client-proto

build:
	$(call msg,Build app,$(APP))
	$(Q)mkdir -p "$(BUILD_DIR)"
	$(Q)go build $(LDFLAGS) -o "$(BUILD_DIR)/$(APP)" ./cmd/$(APP)

fmt:
	$(call msg,Format code,$(APP))
	$(Q)go fmt ./...

check-proto-tools:
	@command -v protoc >/dev/null 2>&1 || { echo "error: protoc not installed"; exit 1; }
	@command -v protoc-gen-go >/dev/null 2>&1 || { echo "error: protoc-gen-go not installed"; exit 1; }
	@command -v protoc-gen-go-grpc >/dev/null 2>&1 || { echo "error: protoc-gen-go-grpc not installed"; exit 1; }

sync-client-proto:
	$(call msg,Sync proto,$(CLIENT_PROTO))
	$(Q)cp "$(PROTO_SRC)" "$(CLIENT_PROTO)"

proto: check-proto-tools sync-client-proto
	$(call msg,Generate proto,$(PROTO_SRC))
	$(Q)protoc -I=. --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative "$(PROTO_SRC)"
	$(call msg,Generate proto,$(CLIENT_PROTO))
	$(Q)protoc -I=.. --go_out=.. --go_opt=paths=source_relative --go-grpc_out=.. --go-grpc_opt=paths=source_relative "$(CLIENT_PROTO)"

test:
	$(call msg,Run unit tests,$(APP))
	$(Q)$(MAKE) -C ../CubeNet/cubevs gen
	$(Q)go test ./...

clean:
	$(call msg,Clean,$(BUILD_DIR))
	$(Q)rm -f "$(BUILD_DIR)/$(APP)"
