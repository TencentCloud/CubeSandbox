FROM ubuntu:24.04 AS builder

ARG OPENRESTY_VERSION=1.29.2.5
ARG NGINX_VERSION=1.29.2

RUN apt update && \
	DEBIAN_FRONTEND=noninteractive apt install -y --no-install-recommends \
		build-essential \
		ca-certificates \
		curl \
		libpcre3-dev \
		libssl-dev \
		make \
		patch \
		perl \
		unzip \
		zlib1g-dev

COPY 0001-nginx-support-TPROXY-listeners-via-transparent-liste.patch /tmp/

RUN cd /tmp && \
	curl -fsSL "https://openresty.org/download/openresty-${OPENRESTY_VERSION}.tar.gz" \
		-o openresty.tar.gz && \
	tar -xzf openresty.tar.gz && \
	cd "openresty-${OPENRESTY_VERSION}" && \
	test -d "bundle/nginx-${NGINX_VERSION}" && \
	patch -p1 < /tmp/0001-nginx-support-TPROXY-listeners-via-transparent-liste.patch && \
	./configure \
		--prefix=/usr/local/openresty \
		--with-pcre-jit && \
	make -j && \
	make install

FROM ubuntu:24.04 AS runtime

ENV PATH=/usr/local/openresty/bin:/usr/local/openresty/nginx/sbin:${PATH}

RUN apt update && \
	DEBIAN_FRONTEND=noninteractive apt install -y --no-install-recommends \
		ca-certificates \
		libpcre3 \
		libssl3 \
		perl-base \
		zlib1g

COPY --from=builder /usr/local/openresty /usr/local/openresty

WORKDIR /usr/local/openresty/nginx

EXPOSE 8080 8443 9090

CMD ["openresty", "-g", "daemon off;"]
