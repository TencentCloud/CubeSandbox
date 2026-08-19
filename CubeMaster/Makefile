ifeq ($(V),1)
Q =
msg =
QUIET =
else
Q = @
QUIET = -q
msg = @printf '  %-8s %s%s\n' "$(1)" "$(notdir $(2))" "$(if $(3), $(3))";
MAKEFLAGS += --no-print-directory
endif

export GO111MODULE=on
export GOOS=linux
GO_BIN_DIR := $(shell if [ -n "$$(go env GOBIN)" ]; then printf "%s" "$$(go env GOBIN)"; else printf "%s/bin" "$$(go env GOPATH)"; fi)
PROTOBUF_INCLUDE_DIR := $(shell python3 -c 'import glob, os, subprocess; candidates = ["/usr/include", "/usr/local/include"]; gopath = subprocess.check_output(["go", "env", "GOPATH"], text=True).strip(); candidates = (glob.glob(os.path.join(gopath, "pkg/mod/github.com/google/gnostic-models@*/third_party")) + glob.glob(os.path.join(gopath, "pkg/mod/github.com/gogo/protobuf@*/protobuf")) + candidates) if gopath else candidates; print(next((c for c in candidates if os.path.exists(os.path.join(c, "google/protobuf/empty.proto")) and os.path.exists(os.path.join(c, "google/protobuf/any.proto"))), ""))')
PROTOC_INCLUDE_FLAGS=-I=.
ifneq ($(strip $(PROTOBUF_INCLUDE_DIR)),)
PROTOC_INCLUDE_FLAGS += -I=$(PROTOBUF_INCLUDE_DIR)
endif
PROTOC_GO_FLAGS=--go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative
export PATH := $(GO_BIN_DIR):$(PATH)

APPS=cubemaster cubemastercli
ROOT_DIR=$(shell pwd)
BUILD_DIR=$(ROOT_DIR)/build
COVERAGE_PROFILE=coverage.profile
DOCKER_BUILD_DIR=docker


PKG=github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base
PROTO=services/errorcode services/cubebox services/images types


# Version — injected at build time via ldflags.
# Priority: env var CUBE_VERSION/CUBE_COMMIT/CUBE_BUILD_TIME > local git/date fallback.
CUBE_VERSION ?= $(shell git describe --tags --abbrev=0 --match 'v*' 2>/dev/null || echo 0.0.0-dev)
CUBE_COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
CUBE_BUILD_TIME ?= $(shell date -u +'%Y-%m-%dT%H:%M:%SZ')

VERSION_PKG=$(PKG)/version
BUILD_FLAGS=-ldflags "-s -w \
  -X '$(VERSION_PKG).Version=$(CUBE_VERSION)' \
  -X '$(VERSION_PKG).Commit=$(CUBE_COMMIT)' \
  -X '$(VERSION_PKG).BuildTime=$(CUBE_BUILD_TIME)'"
ENVD_LOCAL_PATH ?=
ENVD_EMBED_ASSET := $(ROOT_DIR)/cmd/cubemastercli/internal/envd/assets/envd
ENVD_BUILD_TAGS := $(if $(strip $(ENVD_LOCAL_PATH)),embed_envd,)
DATE:=$(shell date +'%Y%m%d-%H%M%S')

NAMESPACE?=cubemaster
QCI_ENV_FILE?=stage-v0.1.0
IMG-Cube-Master=cube-master-v2
OBJ-Cube-Master=cubemaster

APP_VERSION ?= $(CUBE_VERSION)
GIT_COMMIT_SHORT ?= $(shell echo $(CUBE_COMMIT) | cut -c1-7)
TAG?=release-$(APP_VERSION)-$(GIT_COMMIT_SHORT)-$(DATE)
DEVTAG?=dev-$(APP_VERSION)-$(GIT_COMMIT_SHORT)-$(DATE)
# COVERAGE_PACKAGES keeps the default test target focused on unit-style packages.
# Integration tests have their own dedicated entrypoints below.
COVERAGE_PACKAGES=$(shell go list ./... \
	| grep -v github.com/tencentcloud/CubeSandbox/CubeMaster/cmd \
	| grep -v github.com/tencentcloud/CubeSandbox/CubeMaster/integration )


.PHONY: build deps check-proto-tools fmt lint clean
build: $(APPS)
deps:
	$(call msg,Download modules)
	$(Q)go mod download

check-proto-tools:
	@command -v protoc >/dev/null 2>&1 || { echo "error: protoc not installed"; exit 1; }
	@command -v protoc-gen-go >/dev/null 2>&1 || { echo "error: protoc-gen-go not installed"; exit 1; }
	@command -v protoc-gen-go-grpc >/dev/null 2>&1 || { echo "error: protoc-gen-go-grpc not installed"; exit 1; }
	@if [ -z "$(PROTOBUF_INCLUDE_DIR)" ]; then echo "error: protobuf include directory not found"; exit 1; fi

.PHONY: prepare-cubemastercli-envd cleanup-cubemastercli-envd
prepare-cubemastercli-envd:
ifneq ($(strip $(ENVD_LOCAL_PATH)),)
	@test -f "$(ENVD_LOCAL_PATH)" || { echo "error: ENVD_LOCAL_PATH must point to a regular file: $(ENVD_LOCAL_PATH)"; exit 1; }
	@test -s "$(ENVD_LOCAL_PATH)" || { echo "error: ENVD_LOCAL_PATH must not be empty: $(ENVD_LOCAL_PATH)"; exit 1; }
	@size=$$(wc -c < "$(ENVD_LOCAL_PATH)"); \
		if [ "$$size" -gt 16777216 ]; then \
			echo "error: ENVD_LOCAL_PATH exceeds 16MiB: $(ENVD_LOCAL_PATH)"; exit 1; \
		fi
	@magic=$$(LC_ALL=C dd if="$(ENVD_LOCAL_PATH)" bs=4 count=1 2>/dev/null | od -An -tx1 | tr -d ' \n'); \
		if [ "$$magic" != "7f454c46" ]; then \
			echo "error: ENVD_LOCAL_PATH must be an ELF binary: $(ENVD_LOCAL_PATH)"; exit 1; \
		fi
	@mkdir -p "$(dir $(ENVD_EMBED_ASSET))"
	@cp "$(ENVD_LOCAL_PATH)" "$(ENVD_EMBED_ASSET)"
endif

cleanup-cubemastercli-envd:
ifneq ($(strip $(ENVD_LOCAL_PATH)),)
	@rm -f "$(ENVD_EMBED_ASSET)"
endif

$(APPS): deps
	@echo "${GOOS}"
	@echo "${CUBE_VERSION}"
	$(call msg,Build app,$@)
	$(Q)if [ "$@" = "cubemastercli" ] && [ -n "$(strip $(ENVD_LOCAL_PATH))" ]; then \
		$(MAKE) prepare-cubemastercli-envd; \
		trap '$(MAKE) cleanup-cubemastercli-envd' EXIT; \
		go build -tags "$(ENVD_BUILD_TAGS)" -o $(BUILD_DIR)/$@ $(BUILD_FLAGS) cmd/$@/*.go; \
	else \
		go build -o $(BUILD_DIR)/$@ $(BUILD_FLAGS) cmd/$@/*.go; \
	fi



fmt:
	$(Q)go fmt ./...

lint:
	$(Q)go fmt ./...
	$(Q)golangci-lint run


clean:
	@rm -rf docker/cubemaster

.PHONY: test
test:
	$(call msg,Run unit tests)
	$(Q)go clean -testcache
	$(Q)mkdir -p coverage
	@# -gcflags=all=-l disables inlining, which is REQUIRED by the gomonkey-based
	@# tests (e.g. ./pkg/templatecenter): gomonkey redirects a function by
	@# rewriting its machine code, but an inlined call site bypasses the patch, so
	@# a stub intermittently fails to take effect and the real function runs. The
	@# integration targets (testlocal/testtt) already pass this flag.
	@( for pkg in ${COVERAGE_PACKAGES}; do \
		CI=true go test -short -v -gcflags=all=-l -timeout=20m ${TEST_FLAGS} \
			-coverprofile=coverage/unit-test-`echo $$pkg | tr "/" "_"`.cov \
			$$pkg || exit 1 ;\
	done )

.PHONY: coverage
coverage:
	$(Q)echo 'mode: set' > ./coverage/cubemaster.cov
	$(Q)tail -q -n +2 ./coverage/*.cov >> ./coverage/cubemaster.cov
	$(Q)go tool cover -func=./coverage/cubemaster.cov
	$(Q)go tool cover -html=./coverage/cubemaster.cov -o ./coverage/cover.html

view:
	$(call msg,Show coverage report)
	$(Q)go tool cover -html=$(COVERAGE_PROFILE)


testlocal:
	@export CUBE_MASTER_CONFIG_PATH="$(ROOT_DIR)/test/conf.yaml" && go test -v -timeout=24h -gcflags=all=-l  ./integration/... -run=$(tt)

testtt:
	@export CUBE_MASTER_CONFIG_PATH="$(ROOT_DIR)/test/conf.yaml" && go test -v -timeout=24h -gcflags=all=-l ./integration/... -run $(tt)

proto: check-proto-tools
	$(call msg,Generate proto)
	@( for pkg in ${PROTO}; do \
		echo $$pkg ;\
		protoc $(PROTOC_INCLUDE_FLAGS) $(PROTOC_GO_FLAGS) \
		api/$$pkg/v1/*.proto ;\
	done)


BUILDKIT=DOCKER_BUILDKIT=1 docker buildx build

# Example target demonstrating function usage
.PHONY: push-image-dev push-image-product
push-image-dev:
	$(call push-images,dev)

push-image-release:
	$(call push-images,release)
