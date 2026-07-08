#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Single source of truth for the release image tag hard-coded across the
# one-click deployment surface (terraform defaults, systemd launcher, env
# examples, CubeEgress Makefile, install docs).
#
# Run it before tagging a release to bump every hard-coded cube-* component
# image tag to the target version; the release workflow runs it with --check to
# fail fast when any of those defaults drift from the pushed git tag, so a
# published bundle can never reference an image tag that was not built.
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

# Component images that follow the release version. openresty-tproxy is
# deliberately excluded: its tag tracks the OpenResty version, not the release.
COMPONENTS='cube-egress|cube-master|cube-api|cube-proxy|webui|cube-lifecycle-manager'

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

# transform_file <path> -- print the file with its release image tags rewritten
# to ${VERSION}, WITHOUT modifying it. Each entry is anchored so it only touches
# the intended tag and never a go.sum pin, a test fixture, or a changelog entry.
transform_file() {
	local f="$1"
	VER="${VERSION}" perl -pe "$(edit_expr "$f")" "$f"
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
	cube-lifecycle-manager/Makefile)
		echo "s{((?:IMAGE_TAG|CUBE_VERSION)\\s*\\?=\\s*)${PERL_SEMVER}}{\$1\$ENV{VER}}"
		;;
	cube-lifecycle-manager/README.md)
		echo "s{${PERL_SEMVER}}{\$ENV{VER}}g if /cube-lifecycle-manager:|IMAGE_TAG/;"
		;;
	deploy/one-click/scripts/one-click/up-cube-lifecycle-manager.sh)
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
	deploy/one-click/terraform/tencentcloud/variables.tf
	deploy/one-click/terraform/tencentcloud/create.sh
	deploy/one-click/terraform/tencentcloud/build_images.sh
	deploy/one-click/terraform/tencentcloud/env.example
	deploy/one-click/README.md
	deploy/one-click/README_zh.md
	docs/guide/tencentcloud-terraform-deploy.md
	docs/zh/guide/tencentcloud-terraform-deploy.md
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
	# image ref (registry/name:tag) and the tag/version assignment forms
	# (IMAGE_TAG / *_IMAGE_TAG=, CUBE_VERSION, TAG:-).
	local -a patterns=(
		"(${COMPONENTS}):${ERE_SEMVER}"
		"(IMAGE_TAG|CUBE_VERSION|TAG:-).*${ERE_SEMVER}"
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
