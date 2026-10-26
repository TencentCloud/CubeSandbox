#!/bin/bash

export BUILD_TARGET=${BUILD_TARGET-aarch64-unknown-linux-gnu}

WORKLOADS_DIR="$HOME/workloads"

mkdir -p "$WORKLOADS_DIR"

build_edk2() {
    EDK2_BUILD_DIR="$WORKLOADS_DIR/edk2_build"
    EDK2_REPO="https://github.com/tianocore/edk2.git"
    EDK2_DIR="$EDK2_BUILD_DIR/edk2"
    EDK2_PLAT_REPO="https://github.com/tianocore/edk2-platforms.git"
    EDK2_PLAT_DIR="$EDK2_BUILD_DIR/edk2-platforms"
    ACPICA_REPO="https://github.com/acpica/acpica.git"
    ACPICA_DIR="$EDK2_BUILD_DIR/acpica"
    export WORKSPACE="$EDK2_BUILD_DIR"
    export PACKAGES_PATH="$EDK2_DIR:$EDK2_PLAT_DIR"
    export IASL_PREFIX="$ACPICA_DIR/generate/unix/bin/"

    if [ ! -d "$EDK2_BUILD_DIR" ]; then
        mkdir -p "$EDK2_BUILD_DIR"
    fi

    # Prepare source code
    checkout_repo "$EDK2_DIR" "$EDK2_REPO" master "46b4606ba23498d3d0e66b53e498eb3d5d592586"
    pushd "$EDK2_DIR"
    git submodule update --init
    popd
    checkout_repo "$EDK2_PLAT_DIR" "$EDK2_PLAT_REPO" master "8227e9e9f6a8aefbd772b40138f835121ccb2307"
    checkout_repo "$ACPICA_DIR" "$ACPICA_REPO" master "b9c69f81a05c45611c91ea9cbce8756078d76233"

    if [[ ! -f "$EDK2_DIR/.built" || \
          ! -f "$EDK2_PLAT_DIR/.built" || \
          ! -f "$ACPICA_DIR/.built" ]]; then
        pushd "$EDK2_BUILD_DIR"
        # Build
        make -C acpica -j `nproc`
        source edk2/edksetup.sh
        make -C edk2/BaseTools -j `nproc`
        build -a AARCH64 -t GCC5 -p ArmVirtPkg/ArmVirtCloudHv.dsc -b RELEASE -n 0
        cp Build/ArmVirtCloudHv-AARCH64/RELEASE_GCC5/FV/CLOUDHV_EFI.fd "$WORKLOADS_DIR"
        touch "$EDK2_DIR"/.built
        touch "$EDK2_PLAT_DIR"/.built
        touch "$ACPICA_DIR"/.built
        popd
    fi
}

