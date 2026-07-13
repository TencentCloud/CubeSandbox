#!/bin/bash
# CubeProxy container entrypoint.
#
# Layout:
#   - Foreground: openresty/nginx (PID 1's main duty after exec)
#   - Background: crond, log rotation

set -u

/usr/sbin/crond
exec /usr/local/openresty/nginx/sbin/nginx
