#!/usr/bin/env bash
# Point Rust build cache and temp files at /data (override Cursor sandbox /tmp paths).
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export CARGO_TARGET_DIR="${ROOT}/.cargo-target"
export CARGO_HOME="${ROOT}/.cargo-home"
export TMPDIR="${ROOT}/.tmp"
mkdir -p "${CARGO_TARGET_DIR}" "${CARGO_HOME}" "${TMPDIR}"
