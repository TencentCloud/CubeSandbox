#!/usr/bin/env bash
# Prepare the Lima guest for a one-click Cube install. Runs inside the VM as the
# Lima user (uses sudo). Idempotent.
#
#   CUBELET_XFS_SIZE   size of the XFS loop file behind /data/cubelet (default 80G)
#   CUBE_LIMA_HOST_PROXY  http://host:port the docker daemon and git use for
#                      registries/GitHub; default: the http_proxy Lima injected,
#                      if any. Set to "none" to disable.
#   CUBE_LIMA_CN_MIRRORS=1  Go and Cargo mirrors for builds (mainland-China
#                      networks, where proxy.golang.org and crates.io are
#                      unreachable or slow from the VM).
set -euo pipefail

log() { printf '[macos-lima] %s\n' "$*" >&2; }

[[ -e /dev/kvm ]] || { log "no /dev/kvm: nested virtualization is off (needs M3+, macOS 15+, vmType vz)"; exit 1; }

if ! command -v docker >/dev/null; then
  log "installing docker-ce"
  curl -fsSL https://get.docker.com | sudo sh >/dev/null
fi
sudo usermod -aG docker,kvm "$USER"

# install.sh requires /data/cubelet on XFS (reflink clones of template disks).
if ! mountpoint -q /data/cubelet; then
  log "creating ${CUBELET_XFS_SIZE:-80G} XFS loop file for /data/cubelet"
  sudo mkdir -p /data/cubelet
  [[ -f /data/cubelet.img ]] || {
    sudo truncate -s "${CUBELET_XFS_SIZE:-80G}" /data/cubelet.img
    sudo mkfs.xfs -q -m reflink=1 /data/cubelet.img
  }
  grep -q '^/data/cubelet.img ' /etc/fstab ||
    echo '/data/cubelet.img /data/cubelet xfs loop,defaults,nofail 0 0' | sudo tee -a /etc/fstab >/dev/null
  sudo mount /data/cubelet
fi

proxy="${CUBE_LIMA_HOST_PROXY:-${http_proxy:-}}"
[[ "$proxy" == none ]] && proxy=""
node_ip="$(ip -4 -o addr show dev eth0 | awk '{print $4}' | cut -d/ -f1)"
if [[ -n "$proxy" ]]; then
  conf=/etc/systemd/system/docker.service.d/proxy.conf
  want="[Service]
Environment=\"HTTP_PROXY=$proxy\" \"HTTPS_PROXY=$proxy\" \"NO_PROXY=localhost,127.0.0.1,::1,$node_ip,10.0.0.0/8,172.16.0.0/12\""
  # Restarting dockerd restarts Cube's MySQL/Redis/proxy containers: only on change.
  if [[ "$(sudo cat "$conf" 2>/dev/null)" != "$want" ]]; then
    log "docker daemon pulls through $proxy"
    sudo mkdir -p "$(dirname "$conf")"
    printf '%s\n' "$want" | sudo tee "$conf" >/dev/null
    sudo systemctl daemon-reload
    sudo systemctl restart docker
  fi
fi

# The one-click builder container keeps HOME here (Makefile BUILDER_HOME).
builder_home="$HOME/.cache/cube-sandbox-builder"
sudo mkdir -p "$builder_home/.config/go" "$builder_home/.cargo"
{
  printf '[safe]\n\tdirectory = *\n'
  [[ -n "$proxy" ]] && printf '[http "https://github.com/"]\n\tproxy = %s\n' "$proxy"
} | sudo tee "$builder_home/.gitconfig" >/dev/null
if [[ "${CUBE_LIMA_CN_MIRRORS:-0}" == 1 ]]; then
  log "Go/Cargo mirrors for builds"
  printf 'GOPROXY=https://goproxy.cn,direct\nGOSUMDB=sum.golang.google.cn\n' |
    sudo tee "$builder_home/.config/go/env" >/dev/null
  sudo tee "$builder_home/.cargo/config.toml" >/dev/null <<'EOF'
[source.crates-io]
replace-with = "rsproxy-sparse"
[source.rsproxy-sparse]
registry = "sparse+https://rsproxy.cn/index/"
[net]
git-fetch-with-cli = true
EOF
fi
# Built binaries carry no VCS stamp: the checkout is owned by the Lima user,
# the builder runs as root.
grep -q '^GOFLAGS=' "$builder_home/.config/go/env" 2>/dev/null ||
  echo 'GOFLAGS=-buildvcs=false' | sudo tee -a "$builder_home/.config/go/env" >/dev/null

log "ready: $(uname -m), kvm $(stat -c %G /dev/kvm), /data/cubelet $(stat -f -c %T /data/cubelet)"
