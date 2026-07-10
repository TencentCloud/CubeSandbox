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

# Binaries provided by this component. Add new tools under cmd/<name> and
# append the name here; `make build` builds them all.
APPS := cube-snapshot

# Version — injected at build time via ldflags.
# Priority: env var CUBE_VERSION/CUBE_COMMIT/CUBE_BUILD_TIME > local git/date fallback.
CUBE_VERSION ?= $(shell git describe --tags --abbrev=0 --match 'v*' 2>/dev/null || echo 0.0.0-dev)
CUBE_COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
CUBE_BUILD_TIME ?= $(shell date -u +'%Y-%m-%dT%H:%M:%SZ')

VERSION_PKG := github.com/tencentcloud/CubeSandbox/CubeBlobstore/pkg/version
LDFLAGS := -ldflags "-s -w \
  -X '$(VERSION_PKG).Version=$(CUBE_VERSION)' \
  -X '$(VERSION_PKG).Commit=$(CUBE_COMMIT)' \
  -X '$(VERSION_PKG).BuildTime=$(CUBE_BUILD_TIME)'"

.PHONY: build fmt test clean $(APPS)

build: $(APPS)

$(APPS):
	$(call msg,Build app,$@)
	$(Q)mkdir -p "$(BUILD_DIR)"
	$(Q)CGO_ENABLED=0 go build -trimpath $(LDFLAGS) -o "$(BUILD_DIR)/$@" ./cmd/$@

fmt:
	$(call msg,Format code,CubeBlobstore)
	$(Q)go fmt ./...

test:
	$(call msg,Run unit tests,CubeBlobstore)
	$(Q)go test ./...

clean:
	$(call msg,Clean,$(BUILD_DIR))
	$(Q)rm -rf "$(BUILD_DIR)"
