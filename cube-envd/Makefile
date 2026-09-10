# cube-envd — E2B-compatible data-plane daemon running inside CubeSandbox
# sandboxes (Rust).
#
# 根 Makefile 的 `make cube-envd` / `make cubemaster` 通过 builder 容器调用本文件的
# `install` 目标（BINDIR=/workspace/_output/bin）；单独在子目录运行亦可，
# 版本与架构变量均有本地兜底。

# 版本唯一事实源是 src/version.rs 里的 semver 常量（与上游 e2b envd 的 pkg/version.go
# 同构）。这里把它机械提取出来，**仅**供镜像 LABEL / tag 等需要字符串的场合使用；
# 二进制自身的 -version 直接读该常量，构建不再注入版本，因此取值不可能与源码漂移。
CUBE_ENVD_VERSION ?= $(shell sed -n 's/.*CUBE_ENVD_VERSION: &str = "\([^"]*\)".*/\1/p' src/version.rs)
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

.PHONY: build install version version-check fmt lint test proto-gen proto-gen-check proto-doc clean

##TARGET build: build static musl release cube-envd
build:
	CUBE_ENVD_COMMIT=$(CUBE_ENVD_COMMIT) \
		$(CARGO) build --release --locked --target $(TRIPLE) --bin cube-envd

##TARGET install: build and install cube-envd into $(BINDIR)
install: build
	install -D -m 0755 target/$(TRIPLE)/release/cube-envd "$(BINDIR)/cube-envd"

##TARGET version: print the version extracted from src/version.rs
version:
	@echo "$(CUBE_ENVD_VERSION)"

##TARGET version-check: assert the extracted version is a semver and matches the binary
version-check: build
	@printf '%s' "$(CUBE_ENVD_VERSION)" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$$' || { \
		printf '  %-8s %s\n' "FAIL" "src/version.rs does not hold a semver: '$(CUBE_ENVD_VERSION)'" >&2; exit 1; }
	@reported="$$(target/$(TRIPLE)/release/cube-envd -version)"; \
	if [ "$$reported" != "$(CUBE_ENVD_VERSION)" ]; then \
		printf '  %-8s %s\n' "FAIL" "binary reports '$$reported' but src/version.rs holds '$(CUBE_ENVD_VERSION)'" >&2; exit 1; \
	fi
	@printf '  %-8s %s\n' "OK" "envd version $(CUBE_ENVD_VERSION) is a semver and matches the binary"

##TARGET fmt: format the crate sources in place
fmt:
	$(CARGO) fmt

##TARGET lint: formatting and clippy
lint:
	$(CARGO) fmt --check
	$(CARGO) clippy --locked -- -D warnings

##TARGET test: run tests
test:
	$(CARGO) test --locked

# protobuf 绑定由 build.rs 在构建时生成并写回 src/generated/；生成物同时纳入版本控制
# （与 CubeShim/agent 的同类 crate 一致）。构建只在内容变化时写回，于是"构建后工作区
# 有改动"就等价于"提交的生成物已过期"。

##TARGET proto-gen: regenerate src/generated from proto/ (build.rs write-back)
proto-gen:
	$(CARGO) build --locked --quiet --bin cube-envd
	@printf '  %-8s %s\n' "OK" "src/generated regenerated; commit any change"

##TARGET proto-gen-check: fail when src/generated drifts from the generator output
proto-gen-check: proto-gen
	@if ! git diff --quiet -- src/generated; then \
		printf '  %-8s %s\n' "FAIL" "src/generated is out of date; commit the regenerated files" >&2; \
		git --no-pager diff --stat -- src/generated >&2; \
		exit 1; \
	fi
	@printf '  %-8s %s\n' "OK" "src/generated matches the generator output"

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
