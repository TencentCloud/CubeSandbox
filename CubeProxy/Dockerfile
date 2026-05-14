FROM cube-sandbox-image.tencentcloudcr.com/opensource/openresty:1.21.4.1-6-alpine-fat

LABEL maintainer="staryxchen <staryxchen@tencent.com>"

RUN sed -i 's#https\?://dl-cdn.alpinelinux.org#http://mirrors.tencent.com#g' /etc/apk/repositories \
    && mkdir -p /data/log/cube-proxy/ \
    && apk update \
    && apk add --no-cache tzdata curl wget \
    && cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime \
    && echo "Asia/Shanghai" > /etc/timezone

WORKDIR /usr/local/openresty/nginx/
COPY lua/ /usr/local/openresty/nginx/lua/
COPY nginx.conf /usr/local/openresty/nginx/conf/nginx.conf
COPY rotate_nginx_log.sh /usr/local/openresty/nginx/sbin/rotate_nginx_log.sh
COPY root /etc/crontabs/root
COPY start.sh /usr/local/openresty/nginx/sbin/start.sh

EXPOSE 8080 8081

STOPSIGNAL SIGQUIT

ENTRYPOINT ["/usr/local/openresty/nginx/sbin/start.sh"]
