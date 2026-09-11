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

# The layer rule, as the source tree states it:
#
#     app -> {filesystem, process} -> platform -> protocol -> compat
#
# Nothing in the compiler enforces it. `pub(crate)` is reachable from every
# sibling module, and item visibility says nothing about direction, so an edge
# that points the wrong way compiles and every other check in this repo stays
# green. These greps are the enforcement — they run with the unit tests so a
# violation fails the same job that would otherwise pass.
.PHONY: layers
layers:
	@fail=0; \
	check() { \
	  hits=$$(grep -rn --include='*.rs' -E "$$2" $$3 2>/dev/null || true); \
	  if [ -n "$$hits" ]; then \
	    printf 'LAYER VIOLATION: %s\n  no match for /%s/ under: %s\n' "$$1" "$$2" "$$3"; \
	    printf '%s\n' "$$hits" | sed 's/^/    /'; \
	    fail=1; \
	  fi; \
	}; \
	check "a layer below app/ reaches back into it" \
	      'crate::app' 'src/filesystem src/process src/platform src/protocol src/compat'; \
	check "filesystem reaches into process" \
	      'crate::process' 'src/filesystem'; \
	check "process reaches into filesystem" \
	      'crate::filesystem' 'src/process'; \
	check "platform/protocol/compat reach into a domain" \
	      'crate::(filesystem|process)' 'src/platform src/protocol src/compat'; \
	check "protocol or compat reaches up into platform" \
	      'crate::platform' 'src/protocol src/compat'; \
	check "compat reaches up into protocol" \
	      'crate::protocol' 'src/compat'; \
	if [ "$$fail" -ne 0 ]; then \
	  echo "cube-envd: layer rule violated (see the target's comment for the allowed direction)"; \
	  exit 1; \
	fi; \
	echo "cube-envd: layer rule holds (6 gates, 0 references)"
