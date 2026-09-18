# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Component Makefile for cube-envd. Intended to run inside the
# cube-sandbox-builder container; use the repo-root `make cube-envd` /
# `make cube-envd-test` targets from the host.

.PHONY: build
build:
	cargo build --release --locked --target x86_64-unknown-linux-musl

.PHONY: test
test:
	cargo test --locked

.PHONY: fmt
fmt:
	cargo fmt

.PHONY: fmt-check
fmt-check:
	cargo fmt --check

.PHONY: clippy
clippy:
	cargo clippy --all-targets -- -D warnings
