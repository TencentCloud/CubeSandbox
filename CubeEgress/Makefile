IMAGE_TAG              ?= latest
IMAGE_LOCAL             := cube-egress
CUBE_EGRESS_VERSION     ?= v1.0.0
CUBE_EGRESS_COMMIT      ?= $(shell git rev-parse HEAD | cut -c1-8)
CUBE_EGRESS_BUILD_TIME  ?= $(shell date +'%Y%m%d')

IMAGES := \
	cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/cube-egress \
	cube-sandbox-int.tencentcloudcr.com/cube-sandbox/cube-egress \
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
	$(call msg,BUILD IMAGE $(IMAGE_LOCAL):$(IMAGE_TAG))
	$(Q)docker build --rm $(QUIET) --network host \
		--build-arg CUBE_EGRESS_VERSION=$(CUBE_EGRESS_VERSION) \
		--build-arg CUBE_EGRESS_COMMIT=$(CUBE_EGRESS_COMMIT) \
		--build-arg CUBE_EGRESS_BUILD_TIME=$(CUBE_EGRESS_BUILD_TIME) \
		-t $(IMAGE_LOCAL):$(IMAGE_TAG) \
		.

.PHONY: push
push: $(IMAGES)

$(IMAGES):
	$(call msg,TAG and PUSH $@)
	$(Q)docker tag $(IMAGE_LOCAL):$(IMAGE_TAG) $@:$(IMAGE_TAG)
	$(Q)docker push $(QUIET) $@:$(IMAGE_TAG)
