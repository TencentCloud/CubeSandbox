# cube-envd — E2B-compatible data-plane daemon running inside CubeSandbox
# sandboxes (Rust).
#
# 根 Makefile 的 `make cube-envd` / `make cubemaster` 通过 builder 容器调用本文件的
# `install` 目标（BINDIR=/workspace/_output/bin）；单独在子目录运行亦可，
# 版本与架构变量均有本地兜底。

# 版本注入：根 Makefile 构建时通过环境变量覆盖为 CUBE_VERSION / CUBE_COMMIT。
CUBE_ENVD_VERSION ?= $(shell git describe --tags --abbrev=0 --match 'v*' 2>/dev/null || echo 0.0.0-dev)
CUBE_ENVD_COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)

# 与根 Makefile TARGET_ARCH 保持一致。
TARGET_ARCH ?= $(shell uname -m | sed 's/^arm64$$/aarch64/')
TRIPLE := $(TARGET_ARCH)-unknown-linux-musl

# 显式 toolchain：避免 rustup 读取 rust-toolchain.toml 时在 builder-run（非 root）
# 下尝试同步 channel 而写不了 /usr/local/rustup/tmp；与根 Makefile 原内联命令一致。
RUST_TOOLCHAIN ?= 1.89
CARGO := cargo +$(RUST_TOOLCHAIN)

##VAR BINDIR=<path> is the directory for installing the cube-envd executable
BINDIR ?= ../../_output/bin

# 只导出业务 proto；google/ 下的 vendored well-known 类型仅作 import 依赖。
PROTO_DOC_FILES := $(shell find proto -path '*/google/*' -prune -o -name '*.proto' -print | sort)

.PHONY: build install fmt lint test proto-doc clean

##TARGET build: build static musl release cube-envd
build:
	CUBE_ENVD_VERSION=$(CUBE_ENVD_VERSION) CUBE_ENVD_COMMIT=$(CUBE_ENVD_COMMIT) \
		$(CARGO) build --release --locked --target $(TRIPLE) --bin cube-envd

##TARGET install: build and install cube-envd into $(BINDIR)
install: build
	install -D -m 0755 target/$(TRIPLE)/release/cube-envd "$(BINDIR)/cube-envd"

##TARGET fmt: check formatting
fmt:
	$(CARGO) fmt --check

##TARGET lint: formatting and clippy
lint:
	$(CARGO) fmt --check
	$(CARGO) clippy --locked -- -D warnings

##TARGET test: run tests
test:
	$(CARGO) test --locked

##TARGET proto-doc: regenerate the interface doc from .proto (protoc-gen-doc)
proto-doc:
	@mkdir -p doc
	@if command -v protoc-gen-doc >/dev/null 2>&1; then \
		protoc -I=proto --doc_out=doc --doc_opt=markdown,cube-envd-api.md $(PROTO_DOC_FILES); \
	else \
		printf '  %-8s %s\n' "Skip" "proto doc (missing protoc-gen-doc; run inside builder image)"; \
	fi

##TARGET clean: remove build artifacts
clean:
	$(CARGO) clean
