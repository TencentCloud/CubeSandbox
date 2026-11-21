#!/usr/bin/env bash
# Build the one-click bundle for linux/arm64 from this checkout, inside the
# Lima VM, the way release-one-click.yml builds its arm64 bundle: pinned kernel
# and guest image from deploy/release-assets.yaml, the upstream builder image,
# deploy/one-click/build-release-bundle-builder.sh. Prints the bundle path.
#
#   CUBE_LIMA_WEB_DIST  reuse a built web/dist (default: the installed one, if
#                      any); otherwise npm builds it, or without npm a
#                      placeholder page is shipped.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root"
assets="$HOME/cube/assets"
mkdir -p "$assets"

pin() { awk -v k="$1" -F': *' '$1 == k {print $2}' deploy/release-assets.yaml; }
kernel_tag="$(pin kernel_bm_arm64)"
guest_tag="$(pin guest_image)"
base=https://github.com/TencentCloud/CubeSandbox/releases/download
fetch() { [[ -s "$assets/$2" ]] || curl -fsSL --retry 3 -o "$assets/$2" "$base/$1/$3"; }
fetch "$kernel_tag" "vmlinux-arm64-$kernel_tag" vmlinux-arm64
fetch "$kernel_tag" "kernel-metadata-$kernel_tag.json" kernel-metadata.json
fetch "$guest_tag" "cube-guest-image-arm64-$guest_tag.tar.gz" cube-guest-image-arm64.tar.gz

export BUILDER_IMAGE=ghcr.io/tencentcloud/cubesandbox-builder:ubuntu2004
sudo docker image inspect "$BUILDER_IMAGE" >/dev/null 2>&1 || sudo docker pull -q "$BUILDER_IMAGE" >&2

export ONE_CLICK_CUBE_KERNEL_VMLINUX="$assets/vmlinux-arm64-$kernel_tag"
export ONE_CLICK_GUEST_IMAGE_TAR="$assets/cube-guest-image-arm64-$guest_tag.tar.gz"
# Without npm in the VM, ship a
# placeholder dashboard page instead of building it.
web="${CUBE_LIMA_WEB_DIST:-/usr/local/services/cubetoolbox/webui/dist}"
if [[ ! -f "$web/index.html" ]] && ! command -v npm >/dev/null; then
  web="$HOME/cube/webui-placeholder"
  mkdir -p "$web"
  echo '<p>Web UI not built (deploy/macos-lima). Use CubeAPI on :3000.</p>' >"$web/index.html"
fi
[[ -f "$web/index.html" ]] && export ONE_CLICK_WEB_DIST_DIR="$web"
CUBE_VERSION="$(git describe --tags --abbrev=0 --match 'v*')"
export CUBE_VERSION
# Dev builds of one tag must sort by build order, or install.sh refuses the
# next one as a downgrade: a numeric UTC timestamp identifier does that.
ONE_CLICK_DIST_VERSION="${CUBE_VERSION}.$(date -u +%Y%m%d%H%M%S).g$(git rev-parse --short=8 HEAD)-arm64"
export ONE_CLICK_DIST_VERSION
CUBE_BUILD_TIME="$(date -u +'%Y-%m-%dT%H:%M:%SZ')"
export CUBE_BUILD_TIME
eval "$(scripts/read-kernel-metadata.sh "$assets/kernel-metadata-$kernel_tag.json")"
export KERNEL_TAG="${KERNEL_BM_SOURCE_TAG:-}" PVM_KERNEL_TAG="${KERNEL_PVM_SOURCE_TAG:-}"

sudo -E env PATH="$PATH" HOME="$HOME" ./deploy/one-click/build-release-bundle-builder.sh >&2
# shellcheck disable=SC2012 # one known file name
ls -t "$root"/deploy/one-click/dist/cube-sandbox-one-click-"$ONE_CLICK_DIST_VERSION".tar.gz | head -1
