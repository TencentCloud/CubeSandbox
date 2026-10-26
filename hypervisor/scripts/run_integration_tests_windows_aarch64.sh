#!/bin/bash
set -x

source $HOME/.cargo/env
source $(dirname "$0")/test-util.sh
source $(dirname "$0")/common-aarch64.sh

process_common_args "$@"
# For now these values are default for kvm
features=""

# aarch64 not supported for MSHV
if [[ "$hypervisor" = "mshv" ]]; then
    echo "AArch64 is not supported in Microsoft Hypervisor"
    exit 1
fi

WIN_IMAGE_BASENAME="windows-11-iot-enterprise-aarch64.raw"
WIN_IMAGE_FILE="$WORKLOADS_DIR/$WIN_IMAGE_BASENAME"

# Checkout and build EDK2
OVMF_FW="$WORKLOADS_DIR/CLOUDHV_EFI.fd"
build_edk2

BUILD_TARGET="$(uname -m)-unknown-linux-${CH_LIBC}"
CFLAGS=""
TARGET_CC=""
if [[ "${BUILD_TARGET}" == "aarch64-unknown-linux-musl" ]]; then
export TARGET_CC="musl-gcc"
export RUSTFLAGS="-C link-arg=-lgcc -C link_arg=-specs -C link_arg=/usr/lib/aarch64-linux-musl/musl-gcc.specs"
fi

# Check if the images are present
if [[ ! -f ${WIN_IMAGE_FILE} || ! -f ${OVMF_FW} ]]; then
    echo "Windows image/firmware not present in the host"
    exit 1
fi

# Use device mapper to create a snapshot of the Windows image
img_blk_size=$(du -b -B 512 ${WIN_IMAGE_FILE} | awk '{print $1;}')
loop_device=$(losetup --find --show --read-only ${WIN_IMAGE_FILE})
dmsetup create windows-base --table "0 $img_blk_size linear $loop_device 0"
dmsetup mknodes
dmsetup create windows-snapshot-base --table "0 $img_blk_size snapshot-origin /dev/mapper/windows-base"
dmsetup mknodes

export RUST_BACKTRACE=1

cargo build --all --release $features --target $BUILD_TARGET
strip target/$BUILD_TARGET/release/cloud-hypervisor

# Only run with 1 thread to avoid tests interfering with one another because
# Windows has a static IP configured
time cargo test $features "windows::$test_filter" --target $BUILD_TARGET -- ${test_binary_args[*]}
RES=$?

dmsetup remove_all -f
losetup -D

exit $RES
