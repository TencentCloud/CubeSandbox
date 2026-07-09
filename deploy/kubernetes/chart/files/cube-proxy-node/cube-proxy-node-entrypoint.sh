#!/bin/sh
# Cube Proxy Node entrypoint script.
#
# Extracted from the chart-embedded shell in templates/proxy-node.yaml so
# that it can be linted (`shellcheck`), unit-tested, and reviewed as a
# proper source file rather than an inline command list.
#
# Contract:
# Reads the following env vars (populated by the Chart in the Pod spec):
#   CUBE_PROXY_HTTP_LISTEN_PORT   - digits only, target HTTP listen port
#   CUBE_PROXY_HTTPS_LISTEN_PORT  - digits only, target HTTPS listen port
#   CUBE_PROXY_RESOLVER_ADDRS     - space-separated nameservers; empty means
#                                   read from /etc/resolv.conf
#   CUBE_PROXY_RESOLVER_VALID     - nginx `valid=` duration
#   CUBE_PROXY_RESOLVER_TIMEOUT   - nginx `resolver_timeout` value
#   CUBE_PROXY_RESOLVER_IPV6      - on/off
#   REDIS_HOST, REDIS_PORT, REDIS_PASSWORD, REDIS_DB
#   TIMEOUT_MIN, TIMEOUT_MAX
#   NODE_IP, CUBE_SIDECAR_LISTEN_ADDR
# Rewrites nginx.conf listen ports in-place, then generates
# /usr/local/openresty/nginx/conf/global/global.conf and execs start.sh.

set -eu

mkdir -p /usr/local/openresty/nginx/conf/global /data /data/log/cube-proxy /cache

case "${CUBE_PROXY_HTTP_LISTEN_PORT}:${CUBE_PROXY_HTTPS_LISTEN_PORT}" in
  *[!0-9:]*|:*|*:)
    echo "invalid CubeProxy listen ports: http=${CUBE_PROXY_HTTP_LISTEN_PORT} https=${CUBE_PROXY_HTTPS_LISTEN_PORT}" >&2
    exit 1
    ;;
esac

sed -i \
  -e "s/listen 8081 reuseport;/listen ${CUBE_PROXY_HTTP_LISTEN_PORT} reuseport;/g" \
  -e "s/listen 8080 ssl reuseport;/listen ${CUBE_PROXY_HTTPS_LISTEN_PORT} ssl reuseport;/g" \
  -e "s/set \\\$host_proxy_port 8081;/set \\\$host_proxy_port ${CUBE_PROXY_HTTP_LISTEN_PORT};/g" \
  -e "s/set \\\$host_proxy_port 8080;/set \\\$host_proxy_port ${CUBE_PROXY_HTTPS_LISTEN_PORT};/g" \
  /usr/local/openresty/nginx/conf/nginx.conf

escape_nginx_value() {
  printf '%s' "$1" | sed 's/[\\"]/\\&/g'
}

resolver_addrs="${CUBE_PROXY_RESOLVER_ADDRS:-}"
if [ -z "${resolver_addrs}" ]; then
  resolver_addrs="$(awk '/^nameserver[[:space:]]+/ { printf "%s ", $2 }' /etc/resolv.conf | sed 's/[[:space:]]*$//')"
fi
[ -n "${resolver_addrs}" ] || {
  echo "unable to determine nginx DNS resolver for CubeProxy Redis lookups" >&2
  exit 1
}
case "${resolver_addrs}${CUBE_PROXY_RESOLVER_VALID}${CUBE_PROXY_RESOLVER_TIMEOUT}${CUBE_PROXY_RESOLVER_IPV6}" in
  *[\;\{\}\$\`]*)
    echo "invalid CubeProxy resolver configuration" >&2
    exit 1
    ;;
esac

cat > /usr/local/openresty/nginx/conf/global/global.conf <<EOF
resolver ${resolver_addrs} valid=${CUBE_PROXY_RESOLVER_VALID} ipv6=${CUBE_PROXY_RESOLVER_IPV6};
resolver_timeout ${CUBE_PROXY_RESOLVER_TIMEOUT};
set \$redis_ip "$(escape_nginx_value "${REDIS_HOST}")";
set \$redis_port "$(escape_nginx_value "${REDIS_PORT}")";
set \$redis_pd "$(escape_nginx_value "${REDIS_PASSWORD}")";
set \$redis_index "$(escape_nginx_value "${REDIS_DB}")";
set \$timeout_min "$(escape_nginx_value "${TIMEOUT_MIN}")";
set \$timeout_max "$(escape_nginx_value "${TIMEOUT_MAX}")";
set \$cube_proxy_host_ip "$(escape_nginx_value "${NODE_IP}")";
set \$cube_sidecar_addr "$(escape_nginx_value "${CUBE_SIDECAR_LISTEN_ADDR}")";
EOF

exec /usr/local/openresty/nginx/sbin/start.sh
