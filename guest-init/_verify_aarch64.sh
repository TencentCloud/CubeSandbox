#!/bin/bash
set -eux
export RUSTUP_HOME=/home/builder/rustup-user
export CARGO_HOME=/home/builder/.cargo
cd /workspace/guest-init
cargo generate-lockfile
echo '=== cargo tree aarch64 (must not pull x86_64 crate) ==='
if cargo tree --target aarch64-unknown-linux-musl -i x86_64 2>/dev/null | grep -q .; then
  echo "ERROR: x86_64 crate still present for aarch64" >&2
  exit 1
fi
echo 'no x86_64 dep on aarch64 — OK'
echo '=== cargo check aarch64 ==='
cargo check --target aarch64-unknown-linux-musl
echo AARCH64_OK
echo '=== rustup add x86 musl if needed ==='
rustup target add x86_64-unknown-linux-musl || true
echo '=== cargo check x86_64 musl ==='
cargo check --target x86_64-unknown-linux-musl
echo X86_OK
echo '=== cargo test (host) ==='
cargo test
echo TEST_OK
