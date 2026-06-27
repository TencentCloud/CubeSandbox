# Base image is overridable at build time (e.g. to point at a private registry
# mirror or a pinned @sha256 digest). The one-click deploy passes this build
# arg from CUBE_PROXY_BASE_IMAGE in .one-click.env via the compose build.args;
# the default below keeps `docker build CubeProxy/` working standalone.
ARG CUBE_PROXY_BASE_IMAGE=cube-sandbox-image.tencentcloudcr.com/opensource/openresty:1.21.4.1-6-alpine-fat
FROM ${CUBE_PROXY_BASE_IMAGE}

LABEL maintainer="staryxchen <staryxchen@tencent.com>"

RUN sed -i 's#https\?://dl-cdn.alpinelinux.org#http://mirrors.tencent.com#g' /etc/apk/repositories \
    && mkdir -p /data/log/cube-proxy/ \
    && apk update \
    && apk add --no-cache tzdata curl wget \
    && cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime \
    && echo "Asia/Shanghai" > /etc/timezone

WORKDIR /usr/local/openresty/nginx/
COPY lua/ /usr/local/openresty/nginx/lua/
COPY conf/includes/ /usr/local/openresty/nginx/conf/includes/
COPY nginx.conf /usr/local/openresty/nginx/conf/nginx.conf
COPY rotate_nginx_log.sh /usr/local/openresty/nginx/sbin/rotate_nginx_log.sh
COPY root /etc/crontabs/root
COPY start.sh /usr/local/openresty/nginx/sbin/start.sh

# The auto-pause sidecar binary is built outside Docker (so we don't have to
# carry a Go toolchain into the runtime image) and dropped at this exact path
# by either:
#   - CubeProxy/Makefile prebuild-sidecar (in-tree dev path)
#   - deploy/one-click/build-release-bundle.sh (release bundle path; pre-built
#     binary is copied into build-context/bin/cube-proxy-sidecar)
#
# The binary is REQUIRED. Both build flows now refuse to ship a stub — a
# missing or non-statically-linked sidecar would either crash at container
# start or silently hide the lifecycle feature, both of which are worse
# failure modes than failing the docker build right here.
COPY bin/cube-proxy-sidecar /usr/local/openresty/nginx/sbin/cube-proxy-sidecar

EXPOSE 8080 8081

STOPSIGNAL SIGQUIT

ENTRYPOINT ["/usr/local/openresty/nginx/sbin/start.sh"]
