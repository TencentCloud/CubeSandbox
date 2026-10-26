#!/bin/bash

source $HOME/.cargo/env
source $(dirname "$0")/test-util.sh

process_common_args "$@"

BUILD_TARGET=${BUILD_TARGET-x86_64-unknown-linux-gnu}
cargo_args=("")

if [[ $hypervisor = "mshv" ]]; then
    cargo_args+=("--no-default-features")
    cargo_args+=("--features $hypervisor")
elif [[ $(uname -m) = "x86_64" ]]; then
    cargo_args+=("--features tdx")
fi

if [[ "${BUILD_TARGET}" == "aarch64-unknown-linux-musl" ]]; then
    export TARGET_CC="musl-gcc"
    export RUSTFLAGS="-C link-arg=-lgcc -C link_arg=-specs -C link_arg=/usr/lib/aarch64-linux-musl/musl-gcc.specs"
fi

export RUST_BACKTRACE=1
cargo test --lib --bins --target $BUILD_TARGET --workspace ${cargo_args[@]} || exit 1
