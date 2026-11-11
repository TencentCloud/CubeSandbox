#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
SCRIPT="${REPO_ROOT}/deploy/pvm/pvm_setup.sh"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT
OUT="${TMP_DIR}/host/output"
BIN="${TMP_DIR}/bin"
mkdir -p "${OUT}" "${BIN}"
version=6.6.117-45.16.cubesandbox.oc9
for package in kernel kernel-core kernel-modules kernel-devel kernel-headers; do
    : > "${OUT}/${package}-${version}.x86_64.rpm"
done
: > "${OUT}/kernel-debuginfo-${version}.x86_64.rpm"
: > "${OUT}/kernel-source-${version}.noarch.rpm"

cat > "${BIN}/dnf" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" > "${TEST_LOG}"
[[ "${FAIL_INSTALL:-0}" != 1 ]]
EOF
chmod +x "${BIN}/dnf"

run_setup() {
    PATH="${BIN}:/usr/bin:/bin" \
    TEST_LOG="${TMP_DIR}/dnf.log" \
    PVM_SETUP_WORK_DIR="${TMP_DIR}" \
    PVM_SETUP_HOST_BUILD_DIR="${TMP_DIR}/host" \
    PVM_SETUP_FAMILY=rpm \
    PVM_SETUP_SKIP_BUILD=1 \
    PVM_SETUP_SKIP_GUEST_BUILD=1 \
    PVM_SETUP_SKIP_PLACE=1 \
    PVM_SETUP_SKIP_GRUB=1 \
    PVM_SETUP_MODULES_LOAD_DIR="${TMP_DIR}/modules-load.d" \
    PVM_SETUP_ASSUME_YES=1 \
    "$@" bash "${SCRIPT}" --host-only
}

run_setup env >"${TMP_DIR}/success.log" 2>&1
for package in kernel kernel-core kernel-modules kernel-devel kernel-headers; do
    grep -q "${package}-${version}.x86_64.rpm" "${TMP_DIR}/dnf.log"
done
if grep -Eq 'kernel-(debuginfo|source)-' "${TMP_DIR}/dnf.log"; then
    echo "setup attempted to install non-runtime release artifacts" >&2
    exit 1
fi

rm -rf "${TMP_DIR}/modules-load.d"
if run_setup env FAIL_INSTALL=1 >"${TMP_DIR}/failure.log" 2>&1; then
    echo "setup unexpectedly accepted a failed RPM installation" >&2
    exit 1
fi
grep -q 'GRUB and module configuration were not changed' "${TMP_DIR}/failure.log"
if grep -q 'PVM_SETUP_SKIP_GRUB=1, skipping GRUB default switch' "${TMP_DIR}/failure.log"; then
    echo "failed installation still reached GRUB configuration" >&2
    exit 1
fi
[[ ! -e "${TMP_DIR}/modules-load.d/kvm_pvm.conf" ]]

echo "PVM setup RPM tests passed"
