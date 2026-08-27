#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Single source of truth for the release image tag hard-coded across the
# one-click deployment surface (terraform defaults, systemd launcher, env
# examples, CubeEgress Makefile, install docs), the Helm chart defaults
# (deploy/kubernetes/chart/values.yaml component image tags and Chart.yaml
# version / appVersion), and the kubernetes image-build docs / usage examples
# (IMAGE_TAG= / CUBE_VERSION=).
#
# Run it before tagging a release to bump every hard-coded cube-* component
# image tag to the target version; the release workflow runs it with --check to
# fail fast when any of those defaults drift from the pushed git tag, so a
# published bundle / chart can never reference an image tag that was not built.
#
# Usage:
#   scripts/bump-image.sh <version>          # rewrite hard-coded tags to <version>
#   scripts/bump-image.sh --check <version>  # verify everything already equals <version>
#
# <version> is a full release tag like v0.5.0 (matching the git tag /
# ${GITHUB_REF_NAME}). --check additionally scans the whole repo for component
# image references so a NEW hard-coded location that was never added to the list
# below is still caught instead of silently passing.

set -euo pipefail

# semver with an optional -/. suffix (v0.5.0, v0.5.0-rc1). Kept in one place so
# the perl edits and the reverse scan stay in sync.
PERL_SEMVER='v\d+\.\d+\.\d+(?:[-.][0-9A-Za-z.]+)?'
ERE_SEMVER='v[0-9]+\.[0-9]+\.[0-9]+([-.][0-9A-Za-z.]+)?'
# Shared perl sub for IMAGE_TAG=vX / CUBE_VERSION=vX usage examples.
PERL_IMAGE_ASSIGN="s{((?:IMAGE_TAG|CUBE_VERSION)=)${PERL_SEMVER}}{\$1\$ENV{VER}}g"

# Component images that follow the release version (chart + one-click / CI).
# openresty-tproxy is deliberately excluded: its tag tracks the OpenResty
# version, not the release.
COMPONENTS='cube-egress|cube-egress-net|cube-master|cubemastercli|cube-api|cube-ops|cube-proxy|cube-webui|cube-lifecycle-manager|cubelet|cube-shim|cube-kernel|cube-guest|cube-agent|cube-node-init|cube-wait-node-prep|cube-pvm-host-bootstrap'

usage() {
	sed -n '2,/^$/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
	exit "${1:-0}"
}

MODE=bump
case "${1:-}" in
-h | --help) usage 0 ;;
--check)
	MODE=check
	shift
	;;
esac

VERSION="${1:-}"
if [[ -z "${VERSION}" ]]; then
	echo "error: missing <version>" >&2
	usage 2
fi
if [[ ! "${VERSION}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([-.][0-9A-Za-z.]+)?$ ]]; then
	echo "error: version must look like v1.2.3 (got: ${VERSION})" >&2
	exit 2
fi

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"
cd "${repo_root}"

# Helm Chart.yaml version / appVersion omit the leading "v" (SemVer), while
# every image tag keeps it. Strip once so bump and --check stay in sync.
CHART_VERSION="${VERSION#v}"

# transform_file <path> -- print the file with its release image tags rewritten
# to ${VERSION}, WITHOUT modifying it. Each entry is anchored so it only touches
# the intended tag and never a go.sum pin, a test fixture, or a changelog entry.
transform_file() {
	local f="$1"
	VER="${VERSION}" CHART_VER="${CHART_VERSION}" perl -pe "$(edit_expr "$f")" "$f"
}

# edit_expr <path> -- the per-file perl expression used by transform_file.
edit_expr() {
	case "$1" in
	deploy/one-click/scripts/systemd/cube-egress-start.sh)
		# cn/int default image refs + the version named in the header comment.
		echo "s{:${PERL_SEMVER}}{:\$ENV{VER}}g"
		;;
	CubeEgress/Makefile)
		echo "s{((?:IMAGE_TAG|CUBE_VERSION)\\s*\\?=\\s*)${PERL_SEMVER}}{\$1\$ENV{VER}}"
		;;
	cube-lifecycle-manager/Makefile | \
		CubeProxy/Makefile)
		echo "s{((?:IMAGE_TAG|CUBE_VERSION)\\s*\\?=\\s*)${PERL_SEMVER}}{\$1\$ENV{VER}}"
		;;
	cube-lifecycle-manager/README.md)
		echo "s{${PERL_SEMVER}}{\$ENV{VER}}g if /cube-lifecycle-manager:|IMAGE_TAG/;"
		;;
	deploy/one-click/scripts/one-click/up-cube-lifecycle-manager.sh | \
		deploy/one-click/scripts/one-click/up-cube-proxy.sh)
		echo "s{:${PERL_SEMVER}}{:\$ENV{VER}}g"
		;;
	deploy/one-click/terraform/tencentcloud/variables.tf)
		# Only rewrite semvers on image-tag `default` lines (the bare image_tag
		# default and the fully-qualified per-component image defaults), so an
		# unrelated v-prefixed semver added later (e.g. a provider constraint)
		# is left untouched.
		echo "s{${PERL_SEMVER}}{\$ENV{VER}}g if /^\\s*default\\s*=.*(?:\"v\\d|:v\\d)/;"
		;;
	deploy/one-click/terraform/tencentcloud/create.sh | \
		deploy/one-click/README.md | \
		deploy/one-click/README_zh.md | \
		docs/guide/tencentcloud-terraform-deploy.md | \
		docs/zh/guide/tencentcloud-terraform-deploy.md)
		# Only touch the image-tag defaults/examples (:- fallbacks, select_env
		# positional default, generated env template, `TAG=vX`), never other
		# semvers that may live in these files.
		echo "s{${PERL_SEMVER}}{\$ENV{VER}}g if /CUBE_IMAGE_TAG/;"
		;;
	deploy/one-click/terraform/tencentcloud/build_images.sh)
		echo "s{(TAG:-)${PERL_SEMVER}}{\$1\$ENV{VER}}g"
		;;
	deploy/one-click/terraform/tencentcloud/env.example)
		echo "s{${PERL_SEMVER}}{\$ENV{VER}}g if /IMAGE/;"
		;;
	deploy/kubernetes/chart/values.yaml)
		# Only rewrite unquoted release tags (tag: vX.Y.Z). Third-party pins
		# use quoted non-v tags (e.g. "1.28.15", "8.0") and are left alone.
		echo "s{(^\\s+tag:\\s+)${PERL_SEMVER}}{\$1\$ENV{VER}}"
		;;
	deploy/kubernetes/chart/Chart.yaml)
		# Chart package metadata tracks the release without the leading "v".
		# Accept quoted or unquoted forms on read. Write version unquoted and
		# appVersion quoted so Helm treats appVersion as a string, not a float.
		echo "s{(^version:\\s*)\"?\\d+\\.\\d+\\.\\d+(?:[-.][0-9A-Za-z.]+)?\"?}{\$1\$ENV{CHART_VER}}; s{(^appVersion:\\s*)\"?\\d+\\.\\d+\\.\\d+(?:[-.][0-9A-Za-z.]+)?\"?}{\$1\"\$ENV{CHART_VER}\"}"
		;;
	deploy/kubernetes/images/build-cube-images.sh)
		# Usage examples plus the script's own VERSION:- fallback, which
		# IMAGE_TAG and SOURCE_REF inherit when unset.
		echo "${PERL_IMAGE_ASSIGN}; s{(VERSION:-)${PERL_SEMVER}}{\$1\$ENV{VER}}g"
		;;
	deploy/kubernetes/images/README.md)
		# IMAGE_TAG= examples, plus the sentence that names the VERSION default.
		# Leave historical pins alone (e.g. "older release tags such as v0.5.1").
		echo "${PERL_IMAGE_ASSIGN}; s{${PERL_SEMVER}}{\$ENV{VER}}g if /default build/;"
		;;
	deploy/kubernetes/chart/README.md | \
		deploy/one-click/build-guest-image.sh | \
		deploy/one-click/build-agent-ext4.sh | \
		docs/guide/kubernetes/faq.md | \
		docs/zh/guide/kubernetes/faq.md)
		# Docs / usage examples that hard-code IMAGE_TAG=vX or CUBE_VERSION=vX.
		# Leave non-semver placeholders alone (e.g. IMAGE_TAG=dev) and do not
		# touch unrelated tags on the same line (e.g. cube-node:v0.4.0-...).
		echo "${PERL_IMAGE_ASSIGN}"
		;;
	deploy/kubernetes/chart/runtime-values.example.yaml | \
		docs/guide/kubernetes/upgrade.md | \
		docs/zh/guide/kubernetes/upgrade.md)
		# Helm overlay / upgrade examples: `tag: vX` including commented lines.
		echo "s{(tag:\\s+)${PERL_SEMVER}}{\$1\$ENV{VER}}g"
		;;
	*)
		echo "error: no edit rule for $1" >&2
		exit 3
		;;
	esac
}

FILES=(
	deploy/one-click/scripts/systemd/cube-egress-start.sh
	CubeEgress/Makefile
	cube-lifecycle-manager/Makefile
	cube-lifecycle-manager/README.md
	deploy/one-click/scripts/one-click/up-cube-lifecycle-manager.sh
	CubeProxy/Makefile
	deploy/one-click/scripts/one-click/up-cube-proxy.sh
	deploy/one-click/terraform/tencentcloud/variables.tf
	deploy/one-click/terraform/tencentcloud/create.sh
	deploy/one-click/terraform/tencentcloud/build_images.sh
	deploy/one-click/terraform/tencentcloud/env.example
	deploy/one-click/README.md
	deploy/one-click/README_zh.md
	docs/guide/tencentcloud-terraform-deploy.md
	docs/zh/guide/tencentcloud-terraform-deploy.md
	deploy/kubernetes/chart/values.yaml
	deploy/kubernetes/chart/Chart.yaml
	deploy/kubernetes/chart/runtime-values.example.yaml
	deploy/kubernetes/images/build-cube-images.sh
	deploy/kubernetes/images/README.md
	deploy/kubernetes/chart/README.md
	deploy/one-click/build-guest-image.sh
	deploy/one-click/build-agent-ext4.sh
	docs/guide/kubernetes/faq.md
	docs/zh/guide/kubernetes/faq.md
	docs/guide/kubernetes/upgrade.md
	docs/zh/guide/kubernetes/upgrade.md
)

do_bump() {
	local f changed=0
	for f in "${FILES[@]}"; do
		[[ -f "$f" ]] || {
			echo "error: tracked file missing: $f" >&2
			exit 1
		}
		local tmp
		tmp="$(mktemp)"
		transform_file "$f" >"$tmp"
		if ! cmp -s "$f" "$tmp"; then
			cat "$tmp" >"$f"
			echo "bumped ${f}"
			changed=1
		fi
		rm -f "$tmp"
	done
	[[ "$changed" -eq 1 ]] || echo "already at ${VERSION}; nothing to bump"
}

# do_check: (1) every listed file must already equal the bumped output, and
# (2) no component image reference anywhere in the repo may carry a different
# tag -- this catches new hard-coded locations that were never added to FILES.
do_check() {
	local f drift=0

	for f in "${FILES[@]}"; do
		[[ -f "$f" ]] || {
			echo "error: tracked file missing: $f" >&2
			exit 1
		}
		if ! diff -u "$f" <(transform_file "$f") >/dev/null; then
			echo "::error::${f} has image tags that differ from ${VERSION}" >&2
			diff -u "$f" <(transform_file "$f") | sed 's/^/    /' >&2 || true
			drift=1
		fi
	done

	# Reverse scan: catch a release image tag hard-coded in a file that is NOT in
	# FILES. Patterns live in one array so the search and the extraction below stay
	# in sync; they cover the tag formats actually used in this repo: a qualified
	# image ref (registry/name:tag), the tag/version assignment forms
	# (IMAGE_TAG / *_IMAGE_TAG=, CUBE_VERSION, VERSION:-, TAG:-), and Helm
	# `tag: vX` lines. Unquoted `tag: vX` is treated as a Cube release pin by
	# convention (chart values.yaml); third-party pins stay quoted and without
	# a leading "v" (e.g. "1.28.15"). A future unquoted third-party `tag: vX`
	# should be excluded from this scan, not added to FILES (FILES would
	# rewrite it to the Cube release version).
	local -a patterns=(
		"(${COMPONENTS}):${ERE_SEMVER}"
		"(IMAGE_TAG|CUBE_VERSION|VERSION:-|TAG:-).*${ERE_SEMVER}"
		"tag:[[:space:]]*${ERE_SEMVER}"
	)
	local -a grep_args=()
	local p
	for p in "${patterns[@]}"; do grep_args+=(-e "$p"); done

	# Prefer `git grep`: it only searches tracked files, so it is fast and skips
	# build artifacts (e.g. deploy/one-click/.work), node_modules and vendored
	# trees automatically. Fall back to grep when run outside a git work tree.
	local matches
	if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
		matches="$(git grep -nE "${grep_args[@]}" -- . 2>/dev/null || true)"
	else
		matches="$(grep -REn \
			--exclude-dir=.git --exclude-dir=node_modules --exclude-dir=.work \
			--exclude='*.sum' --exclude='*.mod' \
			"${grep_args[@]}" . 2>/dev/null || true)"
	fi
	# Drop references that are intentionally version-pinned fixtures/history.
	matches="$(printf '%s\n' "$matches" |
		grep -vE '(_test\.go|/tests/|/mocks/|/changelog/)' || true)"

	local line tag
	while IFS= read -r line; do
		[[ -z "$line" ]] && continue
		# Pull every semver that sits inside a matched image-tag context on the
		# line and require each to equal the target version.
		while IFS= read -r tag; do
			[[ -n "$tag" && "$tag" != "${VERSION}" ]] || continue
			echo "::error::stray image tag ${tag} (expected ${VERSION}): ${line}" >&2
			drift=1
		done < <(printf '%s' "$line" | grep -oE "${grep_args[@]}" | grep -oE "${ERE_SEMVER}")
	done <<<"$matches"

	if [[ "$drift" -ne 0 ]]; then
		echo "error: image tags are not all at ${VERSION}; run 'scripts/bump-image.sh ${VERSION}'" >&2
		exit 1
	fi
	echo "ok: all release image tags are at ${VERSION}"
}

if [[ "${MODE}" == "check" ]]; then
	do_check
else
	do_bump
fi
