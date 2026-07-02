VERSION     := test
DATE        := $(shell date +'%Y%m%d')
COMMIT      := $(shell git rev-parse HEAD | cut -c1-8)
IMAGE_TAG   := $(VERSION)-$(DATE)-$(COMMIT)

IMAGE_LOCAL     := cube-proxy

# CubeProxy ships an auto-pause sidecar binary inside the same image; we
# pre-build it on the host (via a Go toolchain) before invoking docker build,
# so the runtime image does not have to carry a Go toolchain.
SIDECAR_SRC     := $(CURDIR)/sidecar
SIDECAR_OUT_DIR := $(CURDIR)/bin
SIDECAR_OUT     := $(SIDECAR_OUT_DIR)/cube-proxy-sidecar
# Target architecture for the sidecar binary. Defaults to the host's Go arch
# (so an arm64 build box produces an arm64 binary and an amd64 box produces
# amd64). Override with `make SIDECAR_GOARCH=arm64 ...` for cross-builds. The
# arch MUST match the openresty base image the sidecar is COPY-ed into.
SIDECAR_GOARCH  ?= $(shell go env GOARCH)

ifeq ($(V),1)
	Q =
	msg =
	QUIET =
else
	Q = @
	msg = @printf '  %-8s %s%s\n'					\
		      "$(1)"						\
		      "$(patsubst $(abspath $(OUTPUT))/%,%,$(2))"	\
		      "$(if $(3), $(3))";
	QUIET = -q
	MAKEFLAGS += --no-print-directory
endif

.PHONY: test
test: build

.PHONY: release
release: build

.PHONY: build
build: prebuild-sidecar
	$(call msg,BUILD IMAGE $(IMAGE_LOCAL):$(IMAGE_TAG))
	$(Q)docker build --rm $(QUIET)			\
		-t $(IMAGE_LOCAL):$(IMAGE_TAG)		\
		.

# prebuild-sidecar produces a static linux/$(SIDECAR_GOARCH) binary at
# bin/cube-proxy-sidecar so the Dockerfile can simply COPY it into the
# musl-based openresty:alpine-fat image. The binary MUST be statically
# linked — CGO_ENABLED=0 plus the netgo/osusergo build tags forces the
# pure-Go DNS resolver and pure-Go user lookups, removing the implicit
# dependency on glibc's ld-linux-x86-64.so.2. Without these the binary
# fails at exec with rc=127 / "required file not found" inside Alpine.
#
# Hard requirement: the sidecar source tree must be present and `go` must
# be on PATH. We refuse to fall back to a stub binary — silently shipping
# a placeholder hides real configuration errors and was the source of an
# embarrassing class of "lifecycle just doesn't work" reports. Operators
# building partial checkouts must explicitly opt out via a different
# image flow.
.PHONY: prebuild-sidecar
prebuild-sidecar:
	$(Q)mkdir -p $(SIDECAR_OUT_DIR)
	$(Q)if [ ! -f "$(SIDECAR_SRC)/go.mod" ]; then \
		echo "ERROR: cube-proxy-sidecar source not found at $(SIDECAR_SRC)" >&2; \
		echo "       cube-proxy image cannot be built without it." >&2; \
		exit 1; \
	fi
	$(Q)if ! command -v go >/dev/null 2>&1; then \
		echo "ERROR: 'go' toolchain not found on PATH" >&2; \
		echo "       cube-proxy-sidecar must be built before docker build." >&2; \
		exit 1; \
	fi
	$(call msg,BUILD,cube-proxy-sidecar)
	$(Q)cd "$(SIDECAR_SRC)" && \
		CGO_ENABLED=0 GOOS=linux GOARCH=$(SIDECAR_GOARCH) \
			go build -trimpath -tags 'netgo osusergo' -ldflags '-s -w' \
				-o "$(SIDECAR_OUT)" ./cmd/sidecar

.PHONY: clean
clean:
	$(Q)rm -rf $(SIDECAR_OUT_DIR)
