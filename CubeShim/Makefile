DOCKER_IMAGE_NAME = 

DOCKER_GIT_CRED =
ifneq ($(wildcard $(HOME)/.git-credentials),)
DOCKER_GIT_CRED += -v $(HOME)/.git-credentials:/.git-credentials:ro
endif

UID = $(shell id -u)
GID = $(shell id -g)
PWD = $(shell pwd)

.PHONY: all fmt test
all:
	cargo build --release

fmt:
	cargo fmt

test:
	cargo test --workspace --locked

.PHONY: all-docker
all-docker:
	docker run --rm -i --user "$(UID)":"$(GID)" -v "$(PWD)":/usr/src/myapp $(DOCKER_GIT_CRED) -w /usr/src/myapp/ $(DOCKER_IMAGE_NAME) make all

.PHONY: docker
docker:
	docker run --rm -it --user "$(UID)":"$(GID)" -v "$(PWD)":/usr/src/myapp $(DOCKER_GIT_CRED) -w /usr/src/myapp/ $(DOCKER_IMAGE_NAME) bash
