#!/bin/bash
hypervisor="kvm"
test_filter=""
quick_mode="false"

# Checkout source code of a GIT repo with specified branch and commit
# Args:
#   $1: Target directory
#   $2: GIT URL of the repo
#   $3: Required branch
#   $4: Required commit (optional)
checkout_repo() {
    SRC_DIR="$1"
    GIT_URL="$2"
    GIT_BRANCH="$3"
    GIT_COMMIT="$4"

    # Check whether the local HEAD commit same as the requested commit or not.
    # If commit is not specified, compare local HEAD and remote HEAD.
    # Remove the folder if there is difference.
    if [ -d "$SRC_DIR" ]; then
        pushd $SRC_DIR
        git fetch
        SRC_LOCAL_COMMIT=$(git rev-parse HEAD)
        if [ -z "$GIT_COMMIT" ]; then
            GIT_COMMIT=$(git rev-parse remotes/origin/"$GIT_BRANCH")
        fi
        popd
        if [ "$SRC_LOCAL_COMMIT" != "$GIT_COMMIT" ]; then
            rm -rf "$SRC_DIR"
        fi
    fi

    # Checkout the specified branch and commit (if required)
    if [ ! -d "$SRC_DIR" ]; then
        git clone --depth 1 "$GIT_URL" -b "$GIT_BRANCH" "$SRC_DIR"
        if [ "$GIT_COMMIT" ]; then
            pushd "$SRC_DIR"
            git fetch --depth 1 origin "$GIT_COMMIT"
            git reset --hard FETCH_HEAD
            popd
        fi
    fi
}

build_custom_linux() {
    ARCH=$(uname -m)
    SRCDIR=$PWD
    LINUX_CUSTOM_DIR="$WORKLOADS_DIR/linux-custom"
    LINUX_CUSTOM_BRANCH="ch-5.15.12"
    LINUX_CUSTOM_URL="https://github.com/cloud-hypervisor/linux.git"

    checkout_repo "$LINUX_CUSTOM_DIR" "$LINUX_CUSTOM_URL" "$LINUX_CUSTOM_BRANCH"

    cp $SRCDIR/resources/linux-config-${ARCH} $LINUX_CUSTOM_DIR/.config

    pushd $LINUX_CUSTOM_DIR
    make -j `nproc`
    if [ ${ARCH} == "x86_64" ]; then
       cp vmlinux "$WORKLOADS_DIR/" || exit 1
    elif [ ${ARCH} == "aarch64" ]; then
       cp arch/arm64/boot/Image "$WORKLOADS_DIR/" || exit 1
       cp arch/arm64/boot/Image.gz "$WORKLOADS_DIR/" || exit 1
    fi
    popd
}

cmd_help() {
    echo ""
    echo "Cloud Hypervisor $(basename $0)"
    echo "Usage: $(basename $0) [<args>]"
    echo ""
    echo "Available arguments:"
    echo ""
    echo "    --hypervisor  Underlying hypervisor. Options kvm, mshv"
    echo "    --test-filter Tests to run"
    echo "    --quick       Run only core smoke tests (5 priority levels)"
    echo ""
    echo "    --help        Display this help message."
    echo ""
}

process_common_args() {
    while [ $# -gt 0 ]; do
	case "$1" in
            "-h"|"--help")  { cmd_help; exit 1; } ;;
            "--hypervisor")
                shift
                hypervisor="$1"
                ;;
            "--test-filter")
                shift
                test_filter="$1"
                ;;
            "--quick")
                quick_mode="true"
                ;;
            "--") {
                shift
                break
            } ;;
            *)
                echo "Unknown test scripts argument: $1. Please use '-- --help' for help."
                exit
                ;;
	esac
	shift
    done
    if [[ ! ("$hypervisor" = "kvm" ||  "$hypervisor" = "mshv") ]]; then
        die "Hypervisor value must be kvm or mshv"
    fi

    test_binary_args=($@)
}

# Convert pipe-separated test names into an array of "module::test_name" filters.
# Usage: build_test_filters <module_prefix> <pipe_separated_tests>
# Result is stored in the global array: test_filters
# Example:
#   build_test_filters "common_parallel" "test_a|test_b"
#   => test_filters=("common_parallel::test_a" "common_parallel::test_b")
#
# If pipe_separated_tests is empty, test_filters will contain just the module prefix
# so that all tests under that module are matched.
build_test_filters() {
    local module="$1"
    local tests="$2"
    test_filters=()
    if [ -z "$tests" ]; then
        test_filters=("$module")
    else
        IFS='|' read -ra names <<< "$tests"
        for name in "${names[@]}"; do
            test_filters+=("${module}::${name}")
        done
    fi
}