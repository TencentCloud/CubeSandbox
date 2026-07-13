#!/usr/bin/env bash
# CubeSandbox transparent proxy — phase 1 entrypoint.
#
# Responsibilities (in order):
#   1. Assert CA cert+key exist, are readable, and match each other
#   2. Warn if CA expires within 30 days; fail if already expired
#   3. Assert placeholder cert/key exist (host pre-generates them)
#   4. Assert audit log dir is writable as the cube-proxy worker uid
#   5. nginx -t (config validity)
#   6. exec openresty as PID 1

set -euo pipefail

CA_DIR="/etc/cube/ca"
CA_CERT="${CA_DIR}/cube-root-ca.crt"
CA_KEY="${CA_DIR}/cube-root-ca.key"
PLACEHOLDER_CERT="${CA_DIR}/placeholder.crt"
PLACEHOLDER_KEY="${CA_DIR}/placeholder.key"
AUDIT_DIR="/data/log/cube-egress"
NGINX_BIN="/usr/local/openresty/nginx/sbin/nginx"
NGINX_CONF="/usr/local/openresty/nginx/conf/nginx.conf"

log()   { printf '[entrypoint] %s\n' "$*" >&2; }
fatal() { log "FATAL: $*"; exit 1; }

validate_ipv4_literal() {
    local value="$1"
    local name="${2:-IPv4 address}"
    local a b c d octet

    [[ "${value}" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] \
        || fatal "invalid ${name}: ${value}"
    IFS=. read -r a b c d <<< "${value}"
    for octet in "${a}" "${b}" "${c}" "${d}"; do
        [[ "${octet}" =~ ^[0-9]{1,3}$ ]] || fatal "invalid ${name}: ${value}"
        (( 10#${octet} >= 0 && 10#${octet} <= 255 )) || fatal "invalid ${name}: ${value}"
    done
}

sandbox_gateway_ip_from_cidr() {
    local cidr="$1"
    local ip="${cidr%/*}"
    local mask="${cidr#*/}"
    local a b c d ip_int host_bits mask_int network_int

    [[ "${cidr}" == */* && "${ip}" != "${cidr}" && "${mask}" =~ ^[0-9]+$ ]] \
        || fatal "invalid CUBE_SANDBOX_NETWORK_CIDR: ${cidr}"
    validate_ipv4_literal "${ip}" "CUBE_SANDBOX_NETWORK_CIDR address"
    IFS=. read -r a b c d <<< "${ip}"

    host_bits=$(( 32 - 10#${mask} ))
    (( host_bits >= 1 && host_bits <= 31 )) || fatal "invalid CUBE_SANDBOX_NETWORK_CIDR mask: ${cidr}"
    ip_int=$(( (10#${a} << 24) + (10#${b} << 16) + (10#${c} << 8) + 10#${d} ))
    mask_int=$(( (0xFFFFFFFF << host_bits) & 0xFFFFFFFF ))
    network_int=$(( ip_int & mask_int ))

    printf '%s.%s.%s.%s\n' \
        "$(( ((network_int + 1) >> 24) & 255 ))" \
        "$(( ((network_int + 1) >> 16) & 255 ))" \
        "$(( ((network_int + 1) >> 8) & 255 ))" \
        "$(( (network_int + 1) & 255 ))"
}

configure_listen_ip() {
    local sandbox_network_cidr="${CUBE_SANDBOX_NETWORK_CIDR:-192.168.0.0/18}"
    local listen_ip
    listen_ip="$(sandbox_gateway_ip_from_cidr "${sandbox_network_cidr}")"
    validate_ipv4_literal "${listen_ip}" "CubeEgress listen IP"

    [[ -f "${NGINX_CONF}" ]] || fatal "nginx config missing: ${NGINX_CONF}"
    sed -i -E \
        -e "s/listen [0-9]+\.[0-9]+\.[0-9]+\.[0-9]+:8080 transparent reuseport;/listen ${listen_ip}:8080 transparent reuseport;/" \
        -e "s/listen [0-9]+\.[0-9]+\.[0-9]+\.[0-9]+:8443 ssl transparent reuseport;/listen ${listen_ip}:8443 ssl transparent reuseport;/" \
        "${NGINX_CONF}"
    grep -Fq "listen ${listen_ip}:8080 transparent reuseport;" "${NGINX_CONF}" \
        || fatal "failed to render HTTP listen IP in ${NGINX_CONF}"
    grep -Fq "listen ${listen_ip}:8443 ssl transparent reuseport;" "${NGINX_CONF}" \
        || fatal "failed to render HTTPS listen IP in ${NGINX_CONF}"
    log "nginx transparent listen IP: ${listen_ip}"
}

# -------- 0. Must run as root --------
# Several downstream steps require uid 0:
#   - chown the bind-mounted audit dir (CAP_CHOWN)
#   - nginx master chown'ing temp dirs and setuid()'ing workers
# A `USER` directive in the base image can silently leak through and
# make this container a non-root one; catch that here rather than
# failing later with confusing "Operation not permitted" lines from
# nginx master's mkdir/chown.
my_uid="$(id -u)"
if [[ "${my_uid}" != "0" ]]; then
    fatal "entrypoint must run as root (uid 0), got uid ${my_uid}; \
check Dockerfile USER directive and \`docker run --user\` flag"
fi

# Diagnostic: capability set we actually ended up with. Surfaces missing
# CAP_CHOWN / CAP_SETUID / CAP_SETGID early instead of failing inside
# nginx master with a confusing "Operation not permitted".
if command -v capsh >/dev/null 2>&1; then
    log "caps: $(capsh --print | sed -n 's/^Current: //p')"
elif [[ -r /proc/self/status ]]; then
    log "caps (raw): $(awk '/^Cap(Inh|Prm|Eff|Bnd):/' /proc/self/status | tr '\n' ' ')"
fi

# -------- 1. CA presence --------
[[ -f "${CA_CERT}" ]] || fatal "CA cert missing: ${CA_CERT} (bind-mount ${CA_DIR}?)"
[[ -f "${CA_KEY}"  ]] || fatal "CA key missing:  ${CA_KEY}"
[[ -r "${CA_CERT}" ]] || fatal "CA cert not readable: ${CA_CERT}"
[[ -r "${CA_KEY}"  ]] || fatal "CA key not readable:  ${CA_KEY}"

# -------- 2. CA validity & cert/key match --------
cert_pubhash="$(openssl x509 -in "${CA_CERT}" -noout -pubkey 2>/dev/null \
                  | openssl pkey -pubin -outform DER 2>/dev/null \
                  | openssl dgst -sha256 \
                  || true)"
key_pubhash="$(openssl pkey -in "${CA_KEY}" -pubout -outform DER 2>/dev/null \
                 | openssl dgst -sha256 \
                 || true)"
[[ -n "${cert_pubhash}" ]] || fatal "CA cert unparseable: ${CA_CERT}"
[[ -n "${key_pubhash}"  ]] || fatal "CA key unparseable:  ${CA_KEY}"
[[ "${cert_pubhash}" == "${key_pubhash}" ]] \
    || fatal "CA cert and key do not match"

ca_not_after="$(openssl x509 -in "${CA_CERT}" -noout -enddate | sed 's/^notAfter=//')"
# Use openssl -checkend rather than `date -d` parsing — alpine's busybox
# `date` does not understand the openssl notAfter free-form text
# ("Mon DD HH:MM:SS YYYY GMT") and would silently bypass the expiry check.
if ! openssl x509 -in "${CA_CERT}" -noout -checkend 0 >/dev/null 2>&1; then
    fatal "CA already expired at ${ca_not_after}"
elif ! openssl x509 -in "${CA_CERT}" -noout -checkend $((30 * 86400)) >/dev/null 2>&1; then
    log "WARN: CA expires within 30 days (${ca_not_after}); plan rotation"
else
    log "CA valid until ${ca_not_after}"
fi

# -------- 3. Placeholder cert (must be pre-generated on host) --------
# CA dir is bind-mounted :ro, so generation in-container is not viable.
# nginx parses ssl_certificate at config load and requires a real cert; this
# placeholder is replaced at every TLS handshake by ssl_certificate_by_lua_block.
[[ -f "${PLACEHOLDER_CERT}" ]] || fatal "placeholder cert missing: ${PLACEHOLDER_CERT}; \
pre-generate on host: openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
-days 36500 -nodes -batch -subj /CN=cube-proxy-placeholder -keyout placeholder.key -out placeholder.crt"
[[ -f "${PLACEHOLDER_KEY}"  ]] || fatal "placeholder key missing: ${PLACEHOLDER_KEY}"

# -------- 4. Audit dir writable as worker uid --------
# We chown the directory to uid 8049 here, but we deliberately do NOT
# create access.jsonl: once the dir is owned by 8049, the in-container
# root has no DAC_OVERRIDE under --cap-drop=ALL and cannot create files
# inside it. The cube-proxy worker creates the file itself in
# init_worker_by_lua (audit.lua: open with O_APPEND|O_CREAT).
[[ -d "${AUDIT_DIR}" ]] || fatal "audit dir missing: ${AUDIT_DIR} (bind-mount missing?)"

audit_uid="$(stat -c '%u' "${AUDIT_DIR}")"
if [[ "${audit_uid}" != "8049" ]]; then
    log "audit dir owner is uid ${audit_uid}, expected 8049; chowning"
    chown 8049:8049 "${AUDIT_DIR}" \
        || fatal "chown ${AUDIT_DIR} failed: need --cap-add=CHOWN, or chown 8049:8049 on host"
fi

# Confirm owner has write+exec via mode bits (stat %a → octal like 755 or
# 1755 if sticky/setuid bits set, hence %10 to isolate the owner triad).
audit_mode="$(stat -c '%a' "${AUDIT_DIR}")"
owner_bits=$(( 10#${audit_mode} / 100 % 10 ))
if (( (owner_bits & 3) != 3 )); then
    fatal "audit dir mode ${audit_mode} lacks owner rwx bits: ${AUDIT_DIR}"
fi

# -------- 5. Render listener IP and validate nginx config --------
configure_listen_ip
log "Running nginx -t"
"${NGINX_BIN}" -t

# -------- 6. exec openresty (becomes PID 1) --------
log "Starting openresty (PID 1)"
exec "${NGINX_BIN}" -g "daemon off;"
