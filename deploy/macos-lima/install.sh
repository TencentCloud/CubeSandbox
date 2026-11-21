#!/usr/bin/env bash
# Install a one-click bundle on the Lima VM: install.sh BUNDLE.tar.gz [install.sh args]
# A first install, or `--mode=install` to replace another version, wipes Cube's
# data; the default on an existing install is upgrade.
set -euo pipefail
bundle="$1"; shift
dir="$HOME/cube/installed"
mkdir -p "$dir"
name="$(basename "$bundle" .tar.gz)"
rm -rf "${dir:?}/$name"
tar -xzf "$bundle" -C "$dir"
cd "$dir/$name"
cp env.example .env
cat >>.env <<'EOF'
# Lima's user network is 192.168.5.0/24, inside the default 192.168.0.0/18.
CUBE_SANDBOX_NETWORK_CIDR=10.88.0.0/20
EOF
# Lima injects the host's proxy; the readiness gates must reach local endpoints directly.
node_ip="$(ip -4 -o addr show dev eth0 | awk '{print $4}' | cut -d/ -f1)"
export no_proxy="localhost,127.0.0.1,0.0.0.0,::1,$node_ip" NO_PROXY="localhost,127.0.0.1,0.0.0.0,::1,$node_ip"
sudo -E ./install.sh "$@"
cubemastercli --version
