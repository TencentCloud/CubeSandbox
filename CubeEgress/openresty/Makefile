OPENRESTY_VERSION := 1.29.2.5
NGINX_VERSION     := 1.29.2
IMAGE_LOCAL       := cube-egress/openresty
IMAGE_TAG         := $(OPENRESTY_VERSION)-tproxy

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
	$(call msg,BUILD IMAGE $(IMAGE_LOCAL):$(IMAGE_TAG))
	$(Q)docker build --rm $(QUIET) --network host			\
		--build-arg OPENRESTY_VERSION=$(OPENRESTY_VERSION)	\
		--build-arg NGINX_VERSION=$(NGINX_VERSION)		\
		-t $(IMAGE_LOCAL):$(IMAGE_TAG)				\
		.

.PHONY: push
push: $(IMAGES)

$(IMAGES):
	$(call msg,TAG and PUSH $@)
	$(Q)docker tag $(IMAGE_LOCAL):$(IMAGE_TAG) $@:$(OPENRESTY_VERSION)
	$(Q)docker push $(QUIET) $@:$(OPENRESTY_VERSION)
	$(Q)docker tag $(IMAGE_LOCAL):$(IMAGE_TAG) $@:latest
	$(Q)docker push $(QUIET) $@:latest
