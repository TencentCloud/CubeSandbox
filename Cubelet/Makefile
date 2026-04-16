APPS=cubelet cubecli
COVERAGE_PROFILE=coverage.profile
ROOT_DIR=$(shell pwd)
BUILD_DIR=$(ROOT_DIR)/build
PROTO=services/errorcode services/version services/cubebox services/images services/nbi services/multimetadb types
PACK_DIR=$(BUILD_DIR)/cube-let

export GO111MODULE=on
export GOARCH=amd64
export GOOS=linux
GO_BIN_DIR := $(shell if [ -n "$$(go env GOBIN)" ]; then printf "%s" "$$(go env GOBIN)"; else printf "%s/bin" "$$(go env GOPATH)"; fi)
PROTOBUF_INCLUDE_DIR := $(shell python3 -c 'import glob, os, subprocess; candidates = ["/usr/include", "/usr/local/include"]; gopath = subprocess.check_output(["go", "env", "GOPATH"], text=True).strip(); extras = glob.glob(os.path.join(gopath, "pkg/mod/github.com/google/gnostic-models@*/third_party")) if gopath else []; candidates.extend(extras); print(next((c for c in candidates if os.path.exists(os.path.join(c, "google/protobuf/empty.proto")) and os.path.exists(os.path.join(c, "google/protobuf/any.proto"))), ""))')
PROTOC_INCLUDE_FLAGS=-I=.
ifneq ($(strip $(PROTOBUF_INCLUDE_DIR)),)
PROTOC_INCLUDE_FLAGS += -I=$(PROTOBUF_INCLUDE_DIR)
endif
PROTOC_GO_FLAGS=--go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative
PROTO_DOC_FILES=$(shell python3 -c 'import glob; print(" ".join(sorted(glob.glob("api/**/*.proto", recursive=True))))')
export PATH := $(GO_BIN_DIR):$(PATH)

ifeq ($(V),1)
Q =
msg =
else
Q = @
msg = @printf '  %-8s %s%s\n' "$(1)" "$(notdir $(2))" "$(if $(3), $(3))";
MAKEFLAGS += --no-print-directory
endif

PKG=github.com/tencentcloud/CubeSandbox/Cubelet/pkg

PWD = $(shell pwd)

# COVERAGE_PACKAGES is the coverage we care about.
COVERAGE_PACKAGES=$(shell go list ./... \
	| grep -v github.com/tencentcloud/CubeSandbox/Cubelet/api \
	| grep -v github.com/tencentcloud/CubeSandbox/Cubelet/cmd \
	| grep -v github.com/tencentcloud/CubeSandbox/Cubelet/shimlog \
	| grep -v github.com/tencentcloud/CubeSandbox/Cubelet/runtime \
	| grep -v /integration)

# Version
APP_VERSION=0.4.24
GIT_INIT=$(shell git config --global --add safe.directory $(PWD))
GO_VERSION=$(shell go version | awk '{print $$3}')
GIT_COMMIT=$(shell git rev-parse --short HEAD)
BUILD_DATE=$(shell date +'%Y%m%d-%H:%M:%S')
PROTOC = $(shell protoc --version|awk '{print "protoc.v"$$2}')
VERSION=$(APP_VERSION)-$(GIT_COMMIT)-$(BUILD_DATE)-$(GO_VERSION)-$(PROTOC)
VERSION_PKG=$(PKG)/version
BUILD_FLAGS=-ldflags "-X '$(VERSION_PKG).Version=$(VERSION)'"
PUBLISH_NAME=cubelet-$(APP_VERSION).zip

# use for mark docker image tags
APP_BUILD_VERSION=$(APP_VERSION)-$(GIT_COMMIT)

#
CONF_VERSION=1.1.7
DYNAMIC_CONF_DIR=$(BUILD_DIR)/cube-cubelet-conf
DYNAMIC_PUBLISH_NAME=cube-cubelet-conf-$(CONF_VERSION).zip


.PHONY: build deps check-proto-tools
build: $(APPS)
deps:
	$(call msg,Download modules)
	$(Q)go mod download

check-proto-tools:
	@command -v protoc >/dev/null 2>&1 || { echo "error: protoc not installed"; exit 1; }
	@command -v protoc-gen-go >/dev/null 2>&1 || { echo "error: protoc-gen-go not installed"; exit 1; }
	@command -v protoc-gen-go-grpc >/dev/null 2>&1 || { echo "error: protoc-gen-go-grpc not installed"; exit 1; }
	@if [ -z "$(PROTOBUF_INCLUDE_DIR)" ]; then echo "error: protobuf include directory not found"; exit 1; fi

$(APPS): deps
	@echo "${GOOS}"
	@echo "${VERSION}"
	$(call msg,Build app,$@)
	mkdir -p $(BUILD_DIR)
	$(Q)go build -o $(BUILD_DIR)/$@ \
		$(BUILD_FLAGS) \
		cmd/$@/*.go

pack:build
	$(call msg,Build package)
	$(Q)mkdir -p $(PACK_DIR)/bin $(PACK_DIR)/conf $(PACK_DIR)/scripts
	$(Q)cp -rf $(BUILD_DIR)/cubecli $(PACK_DIR)/bin/
	$(Q)cp -rf $(BUILD_DIR)/cubelet $(PACK_DIR)/bin/
	$(Q)cp ./contrib/unsquashfs-dio $(PACK_DIR)/bin/
	$(Q)cp ./contrib/unsquashfs $(PACK_DIR)/bin/
	$(Q)cp ./contrib/nicl $(PACK_DIR)/bin/
	$(Q)cp ./contrib/cubelet-code-deploy.sh $(PACK_DIR)/bin/
	$(Q)cp -rf config/* $(PACK_DIR)/conf/
	$(Q)tar -czf $(PACK_DIR)/conf/benchmark-tool.tar.gz -C $(PACK_DIR)/conf/ benchmark
	$(Q)rm -rf $(PACK_DIR)/conf/benchmark/
	$(Q)cp -rf scripts/* $(PACK_DIR)/scripts/
	$(Q)mv -f $(PACK_DIR)/conf/plugin.conf $(PACK_DIR)/
	$(Q)rm -f $(BUILD_DIR)/$(PUBLISH_NAME)

	$(Q)cd ./build && zip -q -r  $(BUILD_DIR)/$(PUBLISH_NAME) ./cube-let
	$(Q)rm -rf $(PACK_DIR)

packconf:
	$(Q)mkdir -p $(DYNAMIC_CONF_DIR)
	$(Q)cp -rf dynamicconf/* $(DYNAMIC_CONF_DIR)/
	$(Q)echo $(CONF_VERSION)-$(GIT_COMMIT)-$(BUILD_DATE) > $(DYNAMIC_CONF_DIR)/version
	$(Q)cd ./build && zip -q -r  $(BUILD_DIR)/$(DYNAMIC_PUBLISH_NAME) ./cube-cubelet-conf
	$(Q)rm -rf $(PACK_DIR)

proto: check-proto-tools
	$(call msg,Generate proto)
	@( for pkg in ${PROTO}; do \
		echo $$pkg ;\
		protoc $(PROTOC_INCLUDE_FLAGS) $(PROTOC_GO_FLAGS) \
		api/$$pkg/v1/*.proto ;\
	done)
	@if command -v protoc-gen-doc >/dev/null 2>&1; then \
		printf '  %-8s %s\n' "Generate" "proto doc"; \
		protoc $(PROTOC_INCLUDE_FLAGS) --doc_out=./doc/ --doc_opt=markdown,cubelet-api.md $(PROTO_DOC_FILES); \
	else \
		printf '  %-8s %s\n' "Skip" "proto doc (missing protoc-gen-doc)"; \
	fi

.PHONY: test
test:
	$(call msg,Run unit tests)
	$(Q)go clean -testcache
	$(Q)mkdir -p coverage
	@( for pkg in ${COVERAGE_PACKAGES}; do \
		CI=true go test ${TEST_FLAGS} \
			-coverprofile=coverage/unit-test-`echo $$pkg | tr "/" "_"`.cov \
			$$pkg || exit 1 ;\
	done )
	# todo: add integration test

.PHONY: coverage
coverage:
	$(Q)echo 'mode: set' > ./coverage/cubelet.cov
	$(Q)tail -q -n +2 ./coverage/*.cov >> ./coverage/cubelet.cov
	$(Q)go tool cover -func=./coverage/cubelet.cov
	$(Q)go tool cover -html=./coverage/cubelet.cov -o ./coverage/cover.html

view:
	$(call msg,Show coverage report)
	$(Q)go tool cover -html=$(COVERAGE_PROFILE)

lint:
	$(call msg,Run linters)
	$(Q)go fmt ./...
	$(Q)golangci-lint run

dep:
	$(call msg,Update dependencies)
	$(Q)go mod tidy
	$(Q)go mod download

clean:
	$(call msg,Clean)
	$(Q)rm -rf $(BUILD_DIR)/* $(COVERAGE_PROFILE)
	$(Q)rm -rf coverage/*

.PHONY: unittest
unittest:
	@echo "unittest"

