IMAGE_TAG           ?= v0.6.0
IMAGE_LOCAL         := cube-proxy
CUBE_VERSION        ?= $(IMAGE_TAG)
CUBE_COMMIT         ?= $(shell git rev-parse HEAD | cut -c1-8)
CUBE_BUILD_TIME     ?= $(shell date +'%Y%m%d')

# Base OpenResty image; overridable for private mirrors / pinned digests.
CUBE_PROXY_BASE_IMAGE ?= cube-sandbox-image.tencentcloudcr.com/opensource/openresty:1.21.4.1-6-alpine-fat

# Target architecture; defaults to the host architecture.
# Override with: make build ARCH=amd64  or  make build ARCH=arm64
ARCH ?= $(shell uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')

IMAGES := \
	cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/cube-proxy \
	cube-sandbox-int.tencentcloudcr.com/cube-sandbox/cube-proxy \
	#

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

.PHONY: build
build:
	$(call msg,BUILD IMAGE $(IMAGE_LOCAL):$(IMAGE_TAG)-$(ARCH))
	$(Q)docker build --rm $(QUIET) \
		--platform linux/$(ARCH) \
		--build-arg CUBE_PROXY_BASE_IMAGE=$(CUBE_PROXY_BASE_IMAGE) \
		--build-arg CUBE_VERSION=$(CUBE_VERSION) \
		--build-arg CUBE_COMMIT=$(CUBE_COMMIT) \
		--build-arg CUBE_BUILD_TIME=$(CUBE_BUILD_TIME) \
		-t $(IMAGE_LOCAL):$(IMAGE_TAG)-$(ARCH) \
		.

.PHONY: test
test:
	bash tests/test_start.sh
	bash tests/test_admin_access_log.sh
	bash tests/test_lua_syntax.sh
	@if command -v lua >/dev/null 2>&1; then \
		lua tests/redis_iresty_sentinel_test.lua; \
		lua tests/backend_cache_test.lua; \
		lua tests/sandbox_route_lookup_test.lua; \
	elif command -v luajit >/dev/null 2>&1; then \
		luajit tests/redis_iresty_sentinel_test.lua; \
		luajit tests/backend_cache_test.lua; \
		luajit tests/sandbox_route_lookup_test.lua; \
	else \
		echo "skip lua unit tests (no lua/luajit): redis_iresty_sentinel_test.lua backend_cache_test.lua sandbox_route_lookup_test.lua"; \
	fi

# Push the architecture-specific image to all registries.
# Run `make push` on an amd64 host, then again on an arm64 host, then
# `make manifest` on either host to combine them.
.PHONY: push
push: $(IMAGES)

$(IMAGES):
	$(call msg,TAG and PUSH $@:$(IMAGE_TAG)-$(ARCH))
	$(Q)docker tag $(IMAGE_LOCAL):$(IMAGE_TAG)-$(ARCH) $@:$(IMAGE_TAG)-$(ARCH)
	$(Q)docker push $(QUIET) $@:$(IMAGE_TAG)-$(ARCH)

# Create and push multi-arch manifest lists for $(IMAGE_TAG) and `latest`.
# Requires both amd64 and arm64 images to have been pushed to all registries
# first (via `make push` on each architecture).
.PHONY: manifest
manifest:
	$(Q)for image in $(IMAGES); do \
		for tag in $(IMAGE_TAG) latest; do \
			docker manifest create $$image:$$tag \
				$$image:$(IMAGE_TAG)-amd64 \
				$$image:$(IMAGE_TAG)-arm64; \
			docker manifest push $$image:$$tag; \
			docker manifest rm $$image:$$tag; \
		done; \
	done

.PHONY: clean
clean:
	$(Q)rm -rf bin/
