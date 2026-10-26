#!/bin/bash
set -x

source $HOME/.cargo/env
source $(dirname "$0")/test-util.sh

process_common_args "$@"
# For now these values are default for kvm
features=""

WORKLOADS_DIR="$HOME/workloads"
FOCAL_OS_IMAGE_NAME="focal-server-cloudimg-amd64-custom-20210609-0.raw"
FOCAL_OS_IMAGE="$WORKLOADS_DIR/$FOCAL_OS_IMAGE_NAME"
FW="$WORKLOADS_DIR/hypervisor-fw"
VMLINUX_IMAGE="$WORKLOADS_DIR/vmlinux"

BLK_IMAGE="$WORKLOADS_DIR/blk.img"
MNT_DIR="mount_image"
rm -rf $BLK_IMAGE
pushd $WORKLOADS_DIR
fallocate -l 16M $BLK_IMAGE
mkfs.ext4 -j $BLK_IMAGE
mkdir $MNT_DIR
sudo mount -t ext4 $BLK_IMAGE $MNT_DIR
sudo bash -c "echo bar > $MNT_DIR/foo" || exit 1
sudo umount $BLK_IMAGE
rm -r $MNT_DIR
popd

VFIO_DIR="$WORKLOADS_DIR/vfio"
VFIO_DISK_IMAGE="$WORKLOADS_DIR/vfio.img"
rm -rf $VFIO_DIR $VFIO_DISK_IMAGE
mkdir -p $VFIO_DIR
cp $FOCAL_OS_IMAGE $VFIO_DIR
cp $FW $VFIO_DIR
cp $VMLINUX_IMAGE $VFIO_DIR || exit 1

BUILD_TARGET="$(uname -m)-unknown-linux-${CH_LIBC}"
CFLAGS=""
TARGET_CC=""
if [[ "${BUILD_TARGET}" == "x86_64-unknown-linux-musl" ]]; then
TARGET_CC="musl-gcc"
CFLAGS="-I /usr/include/x86_64-linux-musl/ -idirafter /usr/include/"
fi

cargo build --all --release $features --target $BUILD_TARGET
strip target/$BUILD_TARGET/release/cloud-hypervisor
strip target/$BUILD_TARGET/release/ch-remote

# We always copy a fresh version of our binary for our L2 guest.
cp target/$BUILD_TARGET/release/cloud-hypervisor $VFIO_DIR
cp target/$BUILD_TARGET/release/ch-remote $VFIO_DIR

# test_vfio rely on hugepages
echo 6144 | sudo tee /proc/sys/vm/nr_hugepages
sudo chmod a+rwX /dev/hugepages

export RUST_BACKTRACE=1
time cargo test $features "vfio::test_vfio" -- ${test_binary_args[*]}
RES=$?

if [ $RES -eq 0 ]; then
	time cargo test $features "vfio::test_nvidia" -- --test-threads=1 ${test_binary_args[*]}
	RES=$?
fi

exit $RES
