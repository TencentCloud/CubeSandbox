#!/bin/bash

export CARGO_NET_GIT_FETCH_WITH_CLI=true

echo "install cargo toolchain..."
curl https://sh.rustup.rs -sSf | sh -s -- -y
source "$HOME/.cargo/env"


rustup default 1.74.0
rustc --version

#echo "installing cross tools"
#cargo install cross
#rustup target add aarch64-unknown-linux-gnu
#yum install gcc-aarch64-linux-gnu gcc-c++-aarch64-linux-gnu -y


echo "cargo clippy..."
#cargo rustc --locked --bin cube-hypervisor -- -D warnings -D clippy::undocumented_unsafe_blocks
cargo fmt -- --check
#cross clippy --target=aarch64-unknown-linux-gnu --locked --all --all-targets --no-default-features --tests --examples --features kvm -- -D warnings -D clippy::undocumented_unsafe_blocks
#cross clippy --target=aarch64-unknown-linux-gnu --locked --all --all-targets --tests --examples -- -D warnings -D clippy::undocumented_unsafe_blocks
#cross clippy --target=aarch64-unknown-linux-gnu --locked --all --all-targets --tests --examples --features guest_debug -- -D warnings -D clippy::undocumented_unsafe_blocks
#cross clippy --target=aarch64-unknown-linux-gnu --locked --all --all-targets --tests --examples --features tracing -- -D warnings -D clippy::undocumented_unsafe_blocks
#cross clippy --target=aarch64-unknown-linux-musl --locked --all --all-targets --no-default-features --tests --examples --features kvm -- -D warnings -D clippy::undocumented_unsafe_blocks
#cross clippy --target=aarch64-unknown-linux-musl --locked --all --all-targets --tests --examples -- -D warnings -D clippy::undocumented_unsafe_blocks
#cross clippy --target=aarch64-unknown-linux-musl --locked --all --all-targets --tests --examples --features guest_debug -- -D warnings -D clippy::undocumented_unsafe_blocks
#cross clippy --target=aarch64-unknown-linux-musl --locked --all --all-targets --tests --examples --features tracing -- -D warnings -D clippy::undocumented_unsafe_blocks
cargo clippy --target=x86_64-unknown-linux-gnu --all --all-targets --no-default-features --tests --examples --features kvm -- -D warnings -D clippy::undocumented_unsafe_blocks
cargo clippy --target=x86_64-unknown-linux-gnu --all --all-targets --tests --examples -- -D warnings -D clippy::undocumented_unsafe_blocks
cargo clippy --target=x86_64-unknown-linux-gnu --all --all-targets --tests --examples --features guest_debug -- -D warnings -D clippy::undocumented_unsafe_blocks
cargo clippy --target=x86_64-unknown-linux-gnu --all --all-targets --tests --examples --features tracing -- -D warnings -D clippy::undocumented_unsafe_blocks
cargo clippy --target=x86_64-unknown-linux-gnu --all --all-targets --no-default-features --tests --examples --features mshv -- -D warnings -D clippy::undocumented_unsafe_blocks
cargo clippy --target=x86_64-unknown-linux-gnu --all --all-targets --no-default-features --tests --examples --features mshv,kvm -- -D warnings -D clippy::undocumented_unsafe_blocks
cargo clippy --target=x86_64-unknown-linux-gnu --all --all-targets --no-default-features --tests --examples --features tdx,kvm -- -D warnings -D clippy::undocumented_unsafe_blocks
#cross clippy --target=x86_64-unknown-linux-musl --locked --all --all-targets --no-default-features --tests --examples --features kvm -- -D warnings -D clippy::undocumented_unsafe_blocks
#cross clippy --target=x86_64-unknown-linux-musl --locked --all --all-targets --tests --examples -- -D warnings -D clippy::undocumented_unsafe_blocks
#cross clippy --target=x86_64-unknown-linux-musl --locked --all --all-targets --tests --examples --features guest_debug -- -D warnings -D clippy::undocumented_unsafe_blocks
#cross clippy --target=x86_64-unknown-linux-musl --locked --all --all-targets --tests --examples --features tracing -- -D warnings -D clippy::undocumented_unsafe_blocks
