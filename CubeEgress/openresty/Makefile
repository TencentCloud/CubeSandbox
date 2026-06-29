OPENRESTY_VERSION := 1.29.2.5
NGINX_VERSION     := 1.29.2
IMAGE_LOCAL       := cube-egress/openresty
IMAGE_TAG         := $(OPENRESTY_VERSION)-tproxy

# Target architecture; defaults to the host architecture.
# Override with: make build ARCH=amd64  or  make build ARCH=arm64
ARCH ?= $(shell uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')

IMAGES := \
	cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/openresty-tproxy \
	cube-sandbox-int.tencentcloudcr.com/cube-sandbox/openresty-tproxy \
	#

ifeq ($(V),1)
	Q =
	msg =
	QUIET =
else
	Q = @
	msg = @printf '  %-8s %s%s\n'					\
		      "$(1)"						\
		      "$(2)"						\
		      "$(if $(3), $(3))";
	QUIET = -q
	MAKEFLAGS += --no-print-directory
endif

.PHONY: all
all: build

.PHONY: build
build:
	$(call msg,BUILD IMAGE $(IMAGE_LOCAL):$(IMAGE_TAG)-$(ARCH))
	$(Q)docker build --rm $(QUIET) --network host			\
		--platform linux/$(ARCH)				\
		--build-arg OPENRESTY_VERSION=$(OPENRESTY_VERSION)	\
		--build-arg NGINX_VERSION=$(NGINX_VERSION)		\
		-t $(IMAGE_LOCAL):$(IMAGE_TAG)-$(ARCH)			\
		.

# Push the architecture-specific image to all registries.
# Run `make push` on an amd64 host, then again on an arm64 host.
.PHONY: push
push: $(IMAGES)

$(IMAGES):
	$(call msg,TAG and PUSH $@:$(IMAGE_TAG)-$(ARCH))
	$(Q)docker tag $(IMAGE_LOCAL):$(IMAGE_TAG)-$(ARCH) $@:$(IMAGE_TAG)-$(ARCH)
	$(Q)docker push $(QUIET) $@:$(IMAGE_TAG)-$(ARCH)

# Create and push multi-arch manifest lists for both the version tag and `latest`.
# Requires both amd64 and arm64 images to have been pushed to all registries first.
.PHONY: manifest
manifest:
	$(Q)for image in $(IMAGES); do \
		for tag in $(OPENRESTY_VERSION) latest; do \
			docker manifest create $$image:$$tag \
				$$image:$(IMAGE_TAG)-amd64 \
				$$image:$(IMAGE_TAG)-arm64; \
			docker manifest push $$image:$$tag; \
			docker manifest rm $$image:$$tag; \
		done \
	done
