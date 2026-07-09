VERSION     := test
DATE        := $(shell date +'%Y%m%d')
COMMIT      := $(shell git rev-parse HEAD | cut -c1-8)
IMAGE_TAG   := $(VERSION)-$(DATE)-$(COMMIT)

IMAGE_LOCAL     := cube-proxy

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
build:
	$(call msg,BUILD IMAGE $(IMAGE_LOCAL):$(IMAGE_TAG))
	$(Q)docker build --rm $(QUIET)			\
		-t $(IMAGE_LOCAL):$(IMAGE_TAG)		\
		.
