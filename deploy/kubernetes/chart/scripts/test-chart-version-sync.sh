#!/bin/sh
# Guard: Chart.yaml version / appVersion must match the release tag used by
# component image defaults in values.yaml (without the leading "v").
# Prevents the drift fixed by #1175 from regressing when image tags are bumped
# without updating chart package metadata.
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
REPO_ROOT="$(CDPATH= cd -- "$CHART_DIR/../../.." && pwd)"

component_tags="$(grep -E '^[[:space:]]+tag:[[:space:]]+v[0-9]' "$CHART_DIR/values.yaml" || true)"
[ -n "$component_tags" ] || {
	echo "error: could not find a component image tag (tag: vX.Y.Z) in values.yaml" >&2
	exit 1
}

# Require every Cube component tag to agree before treating any as authoritative.
# Avoid nominating the first tag as "expected" when tags themselves disagree.
tag=""
unique=1
while IFS= read -r line; do
	[ -z "$line" ] && continue
	got="$(printf '%s\n' "$line" | awk '{print $2}')"
	if [ -z "$tag" ]; then
		tag="$got"
	elif [ "$got" != "$tag" ]; then
		unique=0
	fi
done <<EOF
$component_tags
EOF

if [ "$unique" -eq 0 ]; then
	echo "error: values.yaml component tags disagree:" >&2
	while IFS= read -r line; do
		[ -z "$line" ] && continue
		echo "  $line" >&2
	done <<EOF
$component_tags
EOF
	exit 1
fi

chart_ver="${tag#v}"
version="$(awk '/^version:/{gsub(/"/, "", $2); print $2; exit}' "$CHART_DIR/Chart.yaml")"
app_version="$(awk '/^appVersion:/{gsub(/"/, "", $2); print $2; exit}' "$CHART_DIR/Chart.yaml")"

drift=0
if [ "$version" != "$chart_ver" ]; then
	echo "error: Chart.yaml version=$version does not match values.yaml image tag $tag (expected $chart_ver)" >&2
	drift=1
fi
if [ "$app_version" != "$chart_ver" ]; then
	echo "error: Chart.yaml appVersion=$app_version does not match values.yaml image tag $tag (expected $chart_ver)" >&2
	drift=1
fi

if [ "$drift" -ne 0 ]; then
	echo "error: chart metadata out of sync; run 'scripts/bump-image.sh $tag'" >&2
	exit 1
fi

# Exercise the release gate against the derived tag so Chart.yaml stays in
# bump-image.sh's FILES list and rewrite rules.
"$REPO_ROOT/scripts/bump-image.sh" --check "$tag"

echo "ok: Chart.yaml version/appVersion and values.yaml tags are at $tag"
