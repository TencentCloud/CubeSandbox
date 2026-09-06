#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=../start.sh
source "${ROOT_DIR}/start.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT
NGINX_CONF="${TMP_DIR}/nginx.conf"

write_conf() {
    cat > "${NGINX_CONF}" <<'EOF'
            proxy_connect_timeout 10s;
            proxy_send_timeout 60s;
            proxy_read_timeout 60s;
            proxy_connect_timeout 10s;
            proxy_send_timeout 60s;
            proxy_read_timeout 60s;
EOF
}

assert_eq() {
    local got="$1" want="$2" msg="$3"
    [[ "${got}" == "${want}" ]] || {
        printf 'FAIL: %s\ngot:  %s\nwant: %s\n' "${msg}" "${got}" "${want}" >&2
        exit 1
    }
}

assert_eq "$(nginx_timeout_value 300 CUBE_EGRESS_PROXY_READ_TIMEOUT)" 300s "bare integer becomes seconds"
assert_eq "$(nginx_timeout_value 300s CUBE_EGRESS_PROXY_READ_TIMEOUT)" 300s "seconds pass through"
assert_eq "$(nginx_timeout_value 5m CUBE_EGRESS_PROXY_READ_TIMEOUT)" 5m "minutes pass through"

if (nginx_timeout_value 0s CUBE_EGRESS_PROXY_READ_TIMEOUT) >/dev/null 2>&1; then
    printf 'FAIL: 0s should be rejected\n' >&2
    exit 1
fi
if (nginx_timeout_value '300s;rm' CUBE_EGRESS_PROXY_READ_TIMEOUT) >/dev/null 2>&1; then
    printf 'FAIL: injection should be rejected\n' >&2
    exit 1
fi

write_conf
CUBE_EGRESS_PROXY_READ_TIMEOUT=300s
unset CUBE_EGRESS_PROXY_SEND_TIMEOUT
configure_proxy_timeouts
assert_eq "$(grep -cF 'proxy_read_timeout 300s;' "${NGINX_CONF}")" 2 "default read rewrite"
assert_eq "$(grep -cF 'proxy_send_timeout 300s;' "${NGINX_CONF}")" 2 "send follows read"

write_conf
CUBE_EGRESS_PROXY_READ_TIMEOUT=600s
CUBE_EGRESS_PROXY_SEND_TIMEOUT=120s
configure_proxy_timeouts
assert_eq "$(grep -cF 'proxy_read_timeout 600s;' "${NGINX_CONF}")" 2 "independent read"
assert_eq "$(grep -cF 'proxy_send_timeout 120s;' "${NGINX_CONF}")" 2 "independent send"
assert_eq "$(grep -cF 'proxy_connect_timeout 10s;' "${NGINX_CONF}")" 2 "connect timeout untouched"

printf 'start_proxy_timeouts_test: PASS\n'
