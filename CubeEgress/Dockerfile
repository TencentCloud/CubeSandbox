FROM cube-egress/openresty:1.29.2.5-tproxy

RUN apt update && \
	DEBIAN_FRONTEND=noninteractive apt install -y --no-install-recommends \
		bash \
		ca-certificates \
		curl \
		luarocks \
		openssl \
		tzdata

RUN luarocks install lua-resty-http
RUN luarocks install lua-resty-openssl

RUN mkdir -p /data/log/cube-egress /var/run/openresty /etc/cube/ca

RUN groupadd --system --gid 8049 cube-proxy && \
	useradd --system --uid 8049 --gid cube-proxy --no-create-home \
		--shell /usr/sbin/nologin cube-proxy && \
	chown -R cube-proxy:cube-proxy /data/log/cube-egress

WORKDIR /usr/local/openresty/nginx/
COPY lua/ /usr/local/openresty/nginx/lua/
COPY nginx.conf /usr/local/openresty/nginx/conf/nginx.conf
COPY start.sh /usr/local/openresty/nginx/sbin/start.sh

EXPOSE 8080 8443 9090

STOPSIGNAL SIGQUIT

USER root

ENTRYPOINT ["/usr/local/openresty/nginx/sbin/start.sh"]
