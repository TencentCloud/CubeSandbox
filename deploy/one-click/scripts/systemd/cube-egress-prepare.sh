#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Initialise /etc/cube/ca with the four files CubeEgress's start.sh
# requires:
#   cube-root-ca.crt   – the MITM root (10 yr ECDSA P-256)
#   cube-root-ca.key   – the matching private key (mode 0640, root:8049)
#   placeholder.crt    – non-functional cert nginx parses at config load
#   placeholder.key    –   "
#
# CA materials (cube-root-ca.crt / cube-root-ca.key) MUST be identical
# across the cluster: CubeMaster bakes the public cert into every
# template's rootfs, and every CubeEgress instance signs leaf certs
# with the matching private key. Two different CAs would mean a
# template baked on master is not trusted by sandboxes whose traffic
# is signed by a compute-node CubeEgress.
#
# Source-of-truth model:
#   control role  – generate locally if missing
#   compute role  – pull from master via GET /cube/ca/<filename>;
#                   never auto-generate (would diverge from master)
#
# Idempotent: every file is generated/pulled only when missing, so
# re-running this prepare step on an already-initialised host is a
# no-op and does NOT rotate the CA. To rotate, the operator removes
# the files (cluster-wide) and lets the master regenerate.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_root
require_cmd openssl
require_cmd curl

CA_DIR="${CUBE_EGRESS_CA_DIR:-/etc/cube/ca}"
CA_CERT="${CA_DIR}/cube-root-ca.crt"
CA_KEY="${CA_DIR}/cube-root-ca.key"
PH_CERT="${CA_DIR}/placeholder.crt"
PH_KEY="${CA_DIR}/placeholder.key"

# uid 8049 is the cube-proxy worker uid baked into the CubeEgress image
# (matches scripts/cube-proxy-iptables-init.sh PROXY_UID and the
# Dockerfile's `addgroup -g 8049 / adduser -u 8049`). The CA key needs
# to be readable by that uid AT RUNTIME (in the container), so on the
# host we set group=8049 + mode 0640. The host doesn't need a real
# cube-proxy account; only the numeric gid matters once the file is
# bind-mounted into the container.
WORKER_UID="${CUBE_EGRESS_WORKER_UID:-8049}"
WORKER_GID="${CUBE_EGRESS_WORKER_GID:-8049}"

mkdir -p "${CA_DIR}"
chmod 0755 "${CA_DIR}"

# fetch_ca_from_master pulls a single CA file (cert or key) from the
# master node's /cube/ca/<filename> endpoint into a final path with
# matching ownership and permissions. Caller decides which file +
# what mode/group; the function only handles the transport.
#
# Strategy: download to a temp file, then atomic-rename. Avoids leaving
# a half-written CA on disk if curl is interrupted.
fetch_ca_from_master() {
  local filename="$1"  # e.g. cube-root-ca.crt
  local dest="$2"
  local mode="$3"      # e.g. 0644
  local owner="$4"     # e.g. root:root

  local addr url tmp http_code
  addr="$(resolve_control_plane_cubemaster_addr)"
  url="http://${addr}/cube/ca/${filename}"

  tmp="$(mktemp -p "${CA_DIR}" ".${filename}.download.XXXXXX")"
  # shellcheck disable=SC2064
  trap "rm -f '${tmp}'" EXIT

  log "fetching ${url} → ${dest}"
  http_code="$(curl -fsSL --max-time 30 -o "${tmp}" -w '%{http_code}' "${url}" || true)"
  if [[ "${http_code}" != "200" ]]; then
    rm -f "${tmp}"
    trap - EXIT
    die "fetch ${url} failed (HTTP ${http_code:-unknown}); is the master up and the CA generated there?"
  fi

  # Sanity: a zero-byte body means the master served an empty file,
  # which would later trip up CubeEgress's start.sh openssl checks.
  if [[ ! -s "${tmp}" ]]; then
    rm -f "${tmp}"
    trap - EXIT
    die "fetched ${url} is empty"
  fi

  install -m "${mode}" -o "${owner%:*}" -g "${owner#*:}" "${tmp}" "${dest}"
  rm -f "${tmp}"
  trap - EXIT
}

if [[ -f "${CA_CERT}" && -f "${CA_KEY}" ]]; then
  log "CA already present at ${CA_DIR}; leaving as-is"
elif is_compute_role; then
  # Compute role: never generate locally — that would create a CA
  # that diverges from the master's, and templates baked on master
  # would not be trusted by sandboxes whose traffic this node's
  # CubeEgress signs. Pull both files from the master instead.
  log "compute role detected; pulling CA from master"
  fetch_ca_from_master "cube-root-ca.crt" "${CA_CERT}" 0644 "root:root"
  fetch_ca_from_master "cube-root-ca.key" "${CA_KEY}"  0640 "root:${WORKER_GID}"
else
  log "control role; generating CubeEgress root CA at ${CA_DIR}"
  # ECDSA P-256, 10 years, no passphrase. Subject CN identifies the
  # cert as our MITM root in case it ever surfaces in error messages or
  # cert-store inspection inside a sandbox.
  tmp_key="$(mktemp -p "${CA_DIR}" .ca.key.XXXXXX)"
  tmp_crt="$(mktemp -p "${CA_DIR}" .ca.crt.XXXXXX)"
  tmp_cnf="$(mktemp -p "${CA_DIR}" .ca.cnf.XXXXXX)"
  trap 'rm -f "${tmp_key}" "${tmp_crt}" "${tmp_cnf}"' EXIT

  # Minimal config to suppress the system openssl.cnf's default x509v3
  # extensions (specifically [v3_ca]'s basicConstraints + subjectKeyIdentifier).
  # Without this, OpenSSL 1.1.1 doubles them up with our -addext values and
  # produces a certificate that Go's crypto/x509 parser rejects (RFC 5280
  # prohibits duplicate extensions).
  cat > "${tmp_cnf}" <<'EOF'
[req]
distinguished_name = req_distinguished_name
prompt = no
[req_distinguished_name]
EOF

  openssl ecparam -name prime256v1 -genkey -noout -out "${tmp_key}"
  openssl req -x509 -new -key "${tmp_key}" \
    -sha256 -days 3650 \
    -config "${tmp_cnf}" \
    -subj '/CN=CubeSandbox Egress MITM CA' \
    -addext 'basicConstraints=critical,CA:TRUE' \
    -addext 'keyUsage=critical,keyCertSign,cRLSign' \
    -addext 'subjectKeyIdentifier=hash' \
    -out "${tmp_crt}"
  install -m 0644 -o root -g root "${tmp_crt}" "${CA_CERT}"
  install -m 0640 -o root -g "${WORKER_GID}" "${tmp_key}" "${CA_KEY}"
  rm -f "${tmp_key}" "${tmp_crt}" "${tmp_cnf}"
  trap - EXIT
fi

if [[ -f "${PH_CERT}" && -f "${PH_KEY}" ]]; then
  log "placeholder cert already present; leaving as-is"
else
  # Placeholder is local-only: every node generates its own. nginx
  # parses it once at config load, and ssl_certificate_by_lua_block
  # replaces it on every handshake — the placeholder never serves
  # real traffic, so node-divergence here is harmless.
  log "generating placeholder cert at ${CA_DIR}"
  # 100 years so the placeholder never expires while the deployment is
  # alive; CN labels it for triage.
  tmp_pkey="$(mktemp -p "${CA_DIR}" .ph.key.XXXXXX)"
  tmp_pcrt="$(mktemp -p "${CA_DIR}" .ph.crt.XXXXXX)"
  trap 'rm -f "${tmp_pkey}" "${tmp_pcrt}"' EXIT
  openssl req -x509 \
    -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
    -days 36500 -nodes -batch \
    -subj '/CN=cube-proxy-placeholder' \
    -keyout "${tmp_pkey}" \
    -out "${tmp_pcrt}"
  install -m 0644 -o root -g root "${tmp_pcrt}" "${PH_CERT}"
  install -m 0640 -o root -g "${WORKER_GID}" "${tmp_pkey}" "${PH_KEY}"
  rm -f "${tmp_pkey}" "${tmp_pcrt}"
  trap - EXIT
fi

log "CubeEgress CA materials ready under ${CA_DIR}:"
ls -la "${CA_DIR}" >&2

# Also create the audit log dir so the container's start.sh chown step
# is a no-op rather than a fresh chown (and CAP_CHOWN failure path).
AUDIT_DIR="${CUBE_EGRESS_AUDIT_DIR:-/data/log/cube-egress}"
mkdir -p "${AUDIT_DIR}"
chown "${WORKER_UID}:${WORKER_GID}" "${AUDIT_DIR}"
chmod 0755 "${AUDIT_DIR}"
log "audit dir ${AUDIT_DIR} ready (owner ${WORKER_UID}:${WORKER_GID})"

# Wait for network-agent's policy bootstrap endpoint before letting
# cube-egress start. The unit declares
# After=cube-sandbox-network-agent.service, but systemd ordering only
# waits for the network-agent unit to finish activating — not for its
# HTTP listener to bind. cube-egress's lua/init_worker_phase fires
# CUBE_EGRESS_BOOTSTRAP_URL on worker-0 startup; if :19090 isn't ready
# yet, bootstrap_status sticks at "pending" until the next retry tick.
# Block here to make the dependency hard.
NETWORK_AGENT_BOOTSTRAP_URL="${CUBE_EGRESS_BOOTSTRAP_URL:-http://127.0.0.1:19090/v1/policies/dump}"
log "waiting for network-agent bootstrap endpoint ${NETWORK_AGENT_BOOTSTRAP_URL}"
wait_for_http "${NETWORK_AGENT_BOOTSTRAP_URL}" 60 1 \
  || die "network-agent bootstrap endpoint ${NETWORK_AGENT_BOOTSTRAP_URL} not ready in 60s"
