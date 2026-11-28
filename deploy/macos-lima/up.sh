#!/usr/bin/env bash
# Bring up a single-node CubeSandbox in a Lima VM on an Apple Silicon Mac
# (M3 or newer: the VM needs nested virtualization for /dev/kvm). Run on the
# Mac from anywhere in the checkout:
#
#   deploy/macos-lima/up.sh [--bundle FILE|URL] [--reinstall]
#
#   (default)       build the arm64 one-click bundle from the committed HEAD
#                   inside the VM, the way the release job builds it
#   --bundle X      install this arm64 one-click bundle instead of building
#   --reinstall     wipe and reinstall Cube even if this version is installed
#
#   CUBE_LIMA_VM=cube          Lima instance name
#   CUBE_LIMA_CN_MIRRORS=1     Go/Cargo mirrors for in-VM builds (vm-setup.sh)
#
# See docs/guide/macos-lima-deploy.md.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/../.." && pwd)"
vm="${CUBE_LIMA_VM:-cube}"
bundle=""; mode=()
while (($#)); do
  case "$1" in
    --bundle) bundle="$2"; shift 2 ;;
    --reinstall) mode=(--mode=install); shift ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
log() { printf '[macos-lima] %s\n' "$*" >&2; }
in_vm() { limactl shell --workdir / "$vm" -- "$@"; }

[[ "$(uname -s)/$(uname -m)" == Darwin/arm64 ]] || { log "needs an Apple Silicon Mac"; exit 1; }
command -v limactl >/dev/null || { log "install Lima 2.x: brew install lima"; exit 1; }

if ! limactl list -q | grep -qx "$vm"; then
  log "creating Lima VM $vm"
  limactl start --name="$vm" --tty=false "$here/lima.yaml"
elif [[ "$(limactl list --format '{{.Status}}' "$vm")" != Running ]]; then
  limactl start "$vm"
fi

# shellcheck disable=SC2016 # expanded in the VM
guest_home="$(in_vm sh -c 'echo $HOME')"
log "copying $(git -C "$root" rev-parse --short HEAD) into the VM"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
git -C "$root" bundle create "$tmp/src.bundle" HEAD --tags >/dev/null 2>&1
in_vm mkdir -p "$guest_home/cube"
limactl copy "$tmp/src.bundle" "$vm:$guest_home/cube/src.bundle"
in_vm bash -c 'set -e; cd ~/cube
  if [ -d src/.git ]; then git -C src fetch -q ../src.bundle HEAD --tags && git -C src checkout -q --detach FETCH_HEAD
  else git clone -q src.bundle src && git -C src checkout -q --detach; fi'

in_vm env CUBE_LIMA_CN_MIRRORS="${CUBE_LIMA_CN_MIRRORS:-0}" bash "$guest_home/cube/src/deploy/macos-lima/vm-setup.sh"
if [[ -z "$bundle" ]]; then
  log "building the arm64 one-click bundle (first build ~30 min, then ~5 min)"
  bundle_vm="$(in_vm bash "$guest_home/cube/src/deploy/macos-lima/build-bundle.sh")"
elif [[ "$bundle" =~ ^https?:// ]]; then
  bundle_vm="$guest_home/cube/$(basename "$bundle")"
  in_vm curl -fsSL --retry 3 -o "$bundle_vm" "$bundle"
else
  bundle_vm="$guest_home/cube/$(basename "$bundle")"
  limactl copy "$bundle" "$vm:$bundle_vm"
fi
in_vm bash "$guest_home/cube/src/deploy/macos-lima/install.sh" "$bundle_vm" ${mode[@]+"${mode[@]}"}

curl -fsS --noproxy '*' -o /dev/null http://127.0.0.1:13000/health ||
  { log "CubeAPI is not reachable on 127.0.0.1:13000"; exit 1; }
cat <<EOF
CubeSandbox is up in Lima VM "$vm".
  CubeAPI:    http://127.0.0.1:13000   (E2B_API_URL)
  Cube Proxy: 127.0.0.1:18080          (Host: <port>-<sandboxID>.cube.app)
Next: create a template inside the VM (limactl shell $vm), see
docs/guide/macos-lima-deploy.md.
EOF
