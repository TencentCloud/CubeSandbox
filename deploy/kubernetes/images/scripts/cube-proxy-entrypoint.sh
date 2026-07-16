#!/bin/sh
# Cube Proxy entrypoint script.
#
# Extracted from the chart-embedded shell in templates/proxy.yaml so
# that it can be linted (`shellcheck`), unit-tested, and reviewed as a
# proper source file rather than an inline command list.
#
# Contract:
# Reads the following env vars (populated by the Chart in the Pod spec):
#   CUBE_PROXY_HTTP_LISTEN_PORT   - digits only, target HTTP listen port
#   CUBE_PROXY_HTTPS_LISTEN_PORT  - digits only, target HTTPS listen port
#   CUBE_PROXY_ADMIN_LISTEN       - admin server listen address (default 0.0.0.0)
#   CUBE_PROXY_ADMIN_PORT         - admin server port (default 8082)
#   CUBE_PROXY_ADMIN_TOKEN        - optional shared secret for /admin/*
#   CUBE_PROXY_RESOLVER_ADDRS     - space-separated nameservers; empty means
#                                   read from /etc/resolv.conf
#   CUBE_PROXY_RESOLVER_VALID     - nginx `valid=` duration
#   CUBE_PROXY_RESOLVER_TIMEOUT   - nginx `resolver_timeout` value
#   CUBE_PROXY_RESOLVER_IPV6      - on/off
#   REDIS_HOST, REDIS_PORT, REDIS_PASSWORD, REDIS_DB
#   TIMEOUT_MIN, TIMEOUT_MAX
#   NODE_IP, CUBE_SIDECAR_LISTEN_ADDR (cube-lifecycle-manager host:port)
# Rewrites nginx.conf listen ports / admin bind in-place, then generates
# /usr/local/openresty/nginx/conf/global/global.conf and execs start.sh.

set -eu

mkdir -p /usr/local/openresty/nginx/conf/global /data /data/log/cube-proxy /cache

case "${CUBE_PROXY_HTTP_LISTEN_PORT}:${CUBE_PROXY_HTTPS_LISTEN_PORT}" in
  *[!0-9:]*|:*|*:)
    echo "invalid CubeProxy listen ports: http=${CUBE_PROXY_HTTP_LISTEN_PORT} https=${CUBE_PROXY_HTTPS_LISTEN_PORT}" >&2
    exit 1
    ;;
esac

admin_listen="${CUBE_PROXY_ADMIN_LISTEN:-0.0.0.0}"
admin_port="${CUBE_PROXY_ADMIN_PORT:-8082}"
case "${admin_port}" in
  *[!0-9]*|"")
    echo "invalid CubeProxy admin port: ${admin_port}" >&2
    exit 1
    ;;
esac
case "${admin_listen}" in
  *[\;\{\}\$\`\"\\]*)
    echo "invalid CubeProxy admin listen address: ${admin_listen}" >&2
    exit 1
    ;;
esac

sed -i \
  -e "s/listen 8081 reuseport;/listen ${CUBE_PROXY_HTTP_LISTEN_PORT} reuseport;/g" \
  -e "s/listen 8080 ssl reuseport;/listen ${CUBE_PROXY_HTTPS_LISTEN_PORT} ssl reuseport;/g" \
  -e "s/set \\\$host_proxy_port 8081;/set \\\$host_proxy_port ${CUBE_PROXY_HTTP_LISTEN_PORT};/g" \
  -e "s/set \\\$host_proxy_port 8080;/set \\\$host_proxy_port ${CUBE_PROXY_HTTPS_LISTEN_PORT};/g" \
  -e "s/listen 127\\.0\\.0\\.1:8082;/listen ${admin_listen}:${admin_port};/g" \
  /usr/local/openresty/nginx/conf/nginx.conf

escape_nginx_value() {
  printf '%s' "$1" | sed 's/[\\"]/\\&/g'
}

if [ -n "${CUBE_PROXY_ADMIN_TOKEN:-}" ]; then
  token_escaped="$(escape_nginx_value "${CUBE_PROXY_ADMIN_TOKEN}")"
  # Stock nginx.conf sets an empty token in each server block; rewrite them
  # so /admin/* and resume paths share the same secret as cube-lifecycle-manager.
  sed -i \
    -e "s/set \\\$cube_admin_token \"\";/set \\\$cube_admin_token \"${token_escaped}\";/g" \
    /usr/local/openresty/nginx/conf/nginx.conf
fi

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

sidecar_addr="${CUBE_SIDECAR_LISTEN_ADDR:-}"
[ -n "${sidecar_addr}" ] || {
  echo "CUBE_SIDECAR_LISTEN_ADDR is required (cube-lifecycle-manager host:port)" >&2
  exit 1
}

# proxy_registry.lua publishes from ngx.timer, which has no nginx resolver.
# Resolve a hostname REDIS target to an IP up-front when possible.
if [ -n "${CUBE_PROXY_REGISTRY_REDIS_HOST:-}" ]; then
  case "${CUBE_PROXY_REGISTRY_REDIS_HOST}" in
    *[!0-9.]* )
      resolved="$(getent ahostsv4 "${CUBE_PROXY_REGISTRY_REDIS_HOST}" 2>/dev/null | awk 'NR==1 {print $1}')"
      if [ -n "${resolved}" ]; then
        CUBE_PROXY_REGISTRY_REDIS_HOST="${resolved}"
        export CUBE_PROXY_REGISTRY_REDIS_HOST
      fi
      ;;
  esac
fi

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
set \$cube_sidecar_addr "$(escape_nginx_value "${sidecar_addr}")";
set \$cube_admin_token "$(escape_nginx_value "${CUBE_PROXY_ADMIN_TOKEN:-}")";
EOF

exec /usr/local/openresty/nginx/sbin/start.sh
