# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.

CARGO_TARGET_DIR ?= target
export CARGO_TARGET_DIR
.DEFAULT_GOAL := build

build:
	cargo build --locked
clean:
	cargo clean
fmt:
	cargo fmt
fmt-check:
	cargo fmt -- --check
clippy:
	cargo clippy --all-targets --all-features --locked -- -D warnings
check:
	cargo check --all-targets --all-features --locked
test:
	cargo test --all-targets --all-features --locked
ci: fmt-check clippy build test check
.PHONY: build clean fmt fmt-check clippy check test ci
