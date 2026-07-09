#!/usr/bin/env bash
# Coverage for scripts/bump-image.sh: the --check release gate, the bump
# rewrite, the reverse scan, and argument validation. Everything runs inside an
# isolated git repo seeded with copies of the real tracked files, so the test
# never mutates the working tree and stays green across future version bumps.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

# The files bump-image.sh rewrites (must all exist, mirroring its FILES list).
FILES=(
	deploy/one-click/scripts/systemd/cube-egress-start.sh
	CubeEgress/Makefile
	cube-lifecycle-manager/Makefile
	cube-lifecycle-manager/README.md
	deploy/one-click/scripts/one-click/up-cube-lifecycle-manager.sh
	deploy/one-click/terraform/tencentcloud/variables.tf
	deploy/one-click/terraform/tencentcloud/create.sh
	deploy/one-click/terraform/tencentcloud/build_images.sh
	deploy/one-click/terraform/tencentcloud/env.example
	deploy/one-click/README.md
	deploy/one-click/README_zh.md
	docs/guide/tencentcloud-terraform-deploy.md
	docs/zh/guide/tencentcloud-terraform-deploy.md
)

failures=0
fail() {
	echo "FAIL: $*" >&2
	failures=$((failures + 1))
}

# Detect the version the seeded tree currently pins, so the test does not hard
# code a value that future releases will move.
CURRENT="$(grep -oE 'cube-egress:v[0-9]+\.[0-9]+\.[0-9]+([-.][0-9A-Za-z.]+)?' \
	"${REPO_ROOT}/deploy/one-click/scripts/systemd/cube-egress-start.sh" | head -1 | cut -d: -f2)"
[[ -n "${CURRENT}" ]] || {
	echo "FAIL: could not detect current version from cube-egress-start.sh" >&2
	exit 1
}

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT
install -D "${REPO_ROOT}/scripts/bump-image.sh" "${WORK}/scripts/bump-image.sh"
for f in "${FILES[@]}"; do
	install -D "${REPO_ROOT}/${f}" "${WORK}/${f}"
done
(cd "${WORK}" && git init -q && git add -A && git -c user.email=t@t -c user.name=t commit -qm seed)

check() { (cd "${WORK}" && ./scripts/bump-image.sh --check "$1" >/dev/null 2>&1); }
bump() { (cd "${WORK}" && ./scripts/bump-image.sh "$1" >/dev/null 2>&1); }

# 1. clean tree passes at its current version, fails for any other version.
check "${CURRENT}" || fail "--check ${CURRENT} should pass on the seeded tree"
if check v9.9.9; then fail "--check v9.9.9 should fail when files are at ${CURRENT}"; fi

# 2. bump rewrites every format; afterwards the new version passes and the old fails.
bump v0.6.0 || fail "bump v0.6.0 failed"
check v0.6.0 || fail "--check v0.6.0 should pass after bumping"
if check "${CURRENT}"; then fail "--check ${CURRENT} should fail after bumping to v0.6.0"; fi

# 3. reverse scan catches a stray tag in a NEW file that is not in the bump list.
(cd "${WORK}" && printf 'IMAGE_TAG ?= v1.2.3\n' >stray.mk && git add -N stray.mk)
if check v0.6.0; then fail "reverse scan should catch a stray tag in a new file"; fi
(cd "${WORK}" && rm -f stray.mk && git reset -q -- stray.mk 2>/dev/null || true)

# 4. a non-image v-semver is left untouched by bump (variables.tf line guard).
(cd "${WORK}" && printf '\nrequired_version = "~> v1.2.0"\n' \
	>>deploy/one-click/terraform/tencentcloud/variables.tf)
bump v0.7.0 || fail "bump v0.7.0 failed"
if ! grep -q 'required_version = "~> v1.2.0"' \
	"${WORK}/deploy/one-click/terraform/tencentcloud/variables.tf"; then
	fail "bump must not rewrite a non-image semver in variables.tf"
fi

# 5. malformed version is rejected with a usage error (exit 2), not treated as a tag.
rc=0
(cd "${WORK}" && ./scripts/bump-image.sh --check not-a-version >/dev/null 2>&1) || rc=$?
[[ "${rc}" -eq 2 ]] || fail "malformed version should exit 2, got ${rc}"

if [[ "${failures}" -ne 0 ]]; then
	echo "bump-image tests FAILED (${failures})" >&2
	exit 1
fi
echo "bump-image tests OK"
