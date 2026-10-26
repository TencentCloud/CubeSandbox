#!/usr/bin/env bash
# Coverage for scripts/bump-image.sh: the --check release gate, the bump
# rewrite, the reverse scan, and argument validation.
#
# Rewrite / argument checks run inside an isolated git repo seeded with copies
# of the real tracked FILES, so they never mutate the working tree and stay
# green across future version bumps.
#
# The real-repo --check below is an intentional whole-tree consistency gate:
# it catches a NEW hard-coded location that was never added to FILES (the
# isolated seed cannot see those files). CI runs it on a clean checkout; a
# local dirty image-tag pin failing this check is the same as `bump-image.sh
# --check` failing at the repo root.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

failures=0
fail() {
	echo "FAIL: $*" >&2
	failures=$((failures + 1))
}

# Seed every file bump-image.sh rewrites, parsed from its FILES list so the
# two cannot drift. A new rewrite target is then covered by bump/--check
# automatically; add extra assertions below only for format-specific pins.
mapfile -t FILES < <(awk '
	/^FILES=\(/ { in_files=1; next }
	in_files && /^\)/ { exit }
	in_files {
		gsub(/^[ \t]+/, "")
		gsub(/[ \t\\]+$/, "")
		if ($0 != "") print
	}
' "${REPO_ROOT}/scripts/bump-image.sh")
if [[ "${#FILES[@]}" -eq 0 ]]; then
	echo "FAIL: could not parse FILES from bump-image.sh" >&2
	exit 1
fi

# Detect the version the seeded tree currently pins, so the test does not hard
# code a value that future releases will move.
CURRENT="$(grep -oE 'cube-egress:v[0-9]+\.[0-9]+\.[0-9]+([-.][0-9A-Za-z.]+)?' \
	"${REPO_ROOT}/deploy/one-click/scripts/systemd/cube-egress-start.sh" | head -1 | cut -d: -f2)"
[[ -n "${CURRENT}" ]] || {
	echo "FAIL: could not detect current version from cube-egress-start.sh" >&2
	exit 1
}

# Reverse scan against the real worktree (not the isolated seed). This is the
# path that catches a NEW hard-coded location that was never added to FILES.
(cd "${REPO_ROOT}" && ./scripts/bump-image.sh --check "${CURRENT}" >/dev/null) \
	|| fail "real-repo --check ${CURRENT} should pass"

# Synthetic bump target — never a real release tag — so rewrite assertions stay
# meaningful regardless of what CURRENT is. Avoid colliding with CURRENT.
TARGET=v999.99.9
[[ "${CURRENT}" != "${TARGET}" ]] || TARGET=v888.88.8
CHART_VER="${TARGET#v}"

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
if check "${TARGET}"; then fail "--check ${TARGET} should fail when files are at ${CURRENT}"; fi

# 2. bump rewrites every format; afterwards the new version passes and the old fails.
bump "${TARGET}" || fail "bump ${TARGET} failed"
check "${TARGET}" || fail "--check ${TARGET} should pass after bumping"
if check "${CURRENT}"; then fail "--check ${CURRENT} should fail after bumping to ${TARGET}"; fi

# 2b. chart values.yaml component tags move; third-party tags stay put.
component_tags="$(grep -E '^\s+tag:\s+v[0-9]' "${WORK}/deploy/kubernetes/chart/values.yaml" || true)"
[[ -n "${component_tags}" ]] || fail "values.yaml should still have component tag: v… lines after bump"
while IFS= read -r line; do
	[[ -z "${line}" ]] && continue
	got="$(awk '{print $2}' <<<"${line}")"
	[[ "${got}" == "${TARGET}" ]] || fail "values.yaml component tag not bumped: ${line}"
done <<<"${component_tags}"
grep -qE 'tag:\s+"1\.28\.15"' "${WORK}/deploy/kubernetes/chart/values.yaml" \
	|| fail "values.yaml kubectl third-party tag must remain \"1.28.15\""

# 2b-extra. VERSION:- default, README default-build sentence, and YAML tag
# examples move; historical pins in the image-build README stay put.
grep -qE "VERSION=\"\\\$\{VERSION:-${TARGET}\}\"" \
	"${WORK}/deploy/kubernetes/images/build-cube-images.sh" \
	|| fail "build-cube-images.sh VERSION default must become ${TARGET}"
grep -q "so \`${TARGET}\` for the default build" \
	"${WORK}/deploy/kubernetes/images/README.md" \
	|| fail "images README default-build sentence must name ${TARGET}"
grep -q 'older release tags such as `v0.5.1`' \
	"${WORK}/deploy/kubernetes/images/README.md" \
	|| fail "historical v0.5.1 in images README must remain"
grep -qE "CUBE_VERSION=${TARGET}" \
	"${WORK}/deploy/one-click/build-agent-ext4.sh" \
	|| fail "build-agent-ext4.sh usage example must become ${TARGET}"
for f in \
	deploy/kubernetes/chart/runtime-values.example.yaml \
	docs/guide/kubernetes/upgrade.md \
	docs/zh/guide/kubernetes/upgrade.md
do
	grep -qE "tag: ${TARGET}" "${WORK}/${f}" \
		|| fail "${f} tag example must become ${TARGET}"
done

# 2c. Chart.yaml version / appVersion track the release without the leading "v".
grep -qx "version: ${CHART_VER}" "${WORK}/deploy/kubernetes/chart/Chart.yaml" \
	|| fail "Chart.yaml version must become ${CHART_VER} after bump"
grep -qx "appVersion: \"${CHART_VER}\"" "${WORK}/deploy/kubernetes/chart/Chart.yaml" \
	|| fail "Chart.yaml appVersion must become \"${CHART_VER}\" after bump"

# 2d. unquoted appVersion is accepted and normalized to the quoted form on write.
OLD_CHART_VER="${CURRENT#v}"
sed -i -E \
	-e "s/^version:.*/version: ${OLD_CHART_VER}/" \
	-e "s/^appVersion:.*/appVersion: ${OLD_CHART_VER}/" \
	"${WORK}/deploy/kubernetes/chart/Chart.yaml"
bump "${TARGET}" || fail "bump ${TARGET} failed with unquoted appVersion"
grep -qx "version: ${CHART_VER}" "${WORK}/deploy/kubernetes/chart/Chart.yaml" \
	|| fail "Chart.yaml version must become ${CHART_VER} after bump from unquoted appVersion"
grep -qx "appVersion: \"${CHART_VER}\"" "${WORK}/deploy/kubernetes/chart/Chart.yaml" \
	|| fail "Chart.yaml appVersion must become \"${CHART_VER}\" after bump from unquoted form"
check "${TARGET}" || fail "--check ${TARGET} should pass after normalizing unquoted appVersion"

# 2e. quoted version is accepted and normalized to the unquoted form on write.
sed -i -E \
	-e "s/^version:.*/version: \"${OLD_CHART_VER}\"/" \
	-e "s/^appVersion:.*/appVersion: \"${OLD_CHART_VER}\"/" \
	"${WORK}/deploy/kubernetes/chart/Chart.yaml"
# Stale quoted version with already-correct appVersion must not slip past --check.
sed -i -E "s/^appVersion:.*/appVersion: \"${CHART_VER}\"/" \
	"${WORK}/deploy/kubernetes/chart/Chart.yaml"
if check "${TARGET}"; then fail "--check ${TARGET} should fail when version is stale quoted"; fi
bump "${TARGET}" || fail "bump ${TARGET} failed with quoted version"
grep -qx "version: ${CHART_VER}" "${WORK}/deploy/kubernetes/chart/Chart.yaml" \
	|| fail "Chart.yaml version must become ${CHART_VER} (unquoted) after bump from quoted form"
grep -qx "appVersion: \"${CHART_VER}\"" "${WORK}/deploy/kubernetes/chart/Chart.yaml" \
	|| fail "Chart.yaml appVersion must stay \"${CHART_VER}\" after bump from quoted version"
check "${TARGET}" || fail "--check ${TARGET} should pass after normalizing quoted version"

# 3. reverse scan catches a stray tag in a NEW file that is not in the bump list.
(cd "${WORK}" && printf 'IMAGE_TAG ?= v1.2.3\n' >stray.mk && git add -N stray.mk)
if check "${TARGET}"; then fail "reverse scan should catch a stray tag in a new file"; fi
(cd "${WORK}" && rm -f stray.mk && git reset -q -- stray.mk 2>/dev/null || true)

# 3b. reverse scan also catches Helm `tag: vX` outside FILES.
(cd "${WORK}" && printf '    tag: v1.2.3\n' >stray.yaml && git add -N stray.yaml)
if check "${TARGET}"; then fail "reverse scan should catch a stray yaml tag"; fi
(cd "${WORK}" && rm -f stray.yaml && git reset -q -- stray.yaml 2>/dev/null || true)

# 3c. reverse scan also catches VERSION:-vX outside FILES (the form the
# v0.6.0 bump missed in build-cube-images.sh).
(cd "${WORK}" && printf 'VERSION="${VERSION:-v1.2.3}"\n' >stray.sh && git add -N stray.sh)
if check "${TARGET}"; then fail "reverse scan should catch a stray VERSION:- tag"; fi
(cd "${WORK}" && rm -f stray.sh && git reset -q -- stray.sh 2>/dev/null || true)

# 4. a non-image v-semver is left untouched by bump (variables.tf line guard).
(cd "${WORK}" && printf '\nrequired_version = "~> v1.2.0"\n' \
	>>deploy/one-click/terraform/tencentcloud/variables.tf)
bump "${TARGET}" || fail "bump ${TARGET} failed"
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
