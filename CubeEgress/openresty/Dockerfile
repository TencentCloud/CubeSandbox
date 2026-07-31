# Shared base carrying the (optional) apt-mirror rewrite so both the builder and
# runtime stages inherit the patched sources.list without duplicating the logic.
FROM ubuntu:24.04 AS base

# Base URL of a China-reachable Ubuntu apt mirror (set via `make build MIRROR=cn`).
# amd64 packages resolve under ${APT_MIRROR_BASE}/ubuntu, arm64 under
# ${APT_MIRROR_BASE}/ubuntu-ports; empty leaves the image default (archive.ubuntu.com
# on amd64, ports.ubuntu.com on arm64).
ARG APT_MIRROR_BASE=
# Skip the mirror rewrite when GITHUB_ACTIONS=true so CI always builds against
# upstream, matching docker/Dockerfile.builder.
ARG GITHUB_ACTIONS=false

# Must stay http:// -- `apt update` below runs before ca-certificates is installed,
# so TLS is not yet available and an https mirror would fail to verify.
RUN set -eux; \
	if [ "${GITHUB_ACTIONS}" != "true" ] && [ -n "${APT_MIRROR_BASE}" ]; then \
		case "${APT_MIRROR_BASE}" in \
			http://*) ;; \
			*) echo "APT_MIRROR_BASE must be an http:// URL (ca-certificates is not yet installed): ${APT_MIRROR_BASE}" >&2; exit 1;; \
		esac; \
		case "${APT_MIRROR_BASE}" in \
			*[\$\`\"\;\!\&\|\\]*) echo "APT_MIRROR_BASE contains characters unsafe for sed: ${APT_MIRROR_BASE}" >&2; exit 1;; \
		esac; \
		target=/etc/apt/sources.list.d/ubuntu.sources; \
		if [ -f "${target}" ]; then \
			sed -i "s|http://archive.ubuntu.com/ubuntu|${APT_MIRROR_BASE}/ubuntu|g; \
				s|http://security.ubuntu.com/ubuntu|${APT_MIRROR_BASE}/ubuntu|g; \
				s|http://ports.ubuntu.com/ubuntu-ports|${APT_MIRROR_BASE}/ubuntu-ports|g" \
				"${target}"; \
		else \
			echo "WARNING: ${target} not found; apt mirror rewrite skipped" >&2; \
		fi; \
	fi

FROM base AS builder

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

FROM base AS runtime

ENV PATH=/usr/local/openresty/bin:/usr/local/openresty/nginx/sbin:${PATH}

RUN apt update && \
	DEBIAN_FRONTEND=noninteractive apt install -y --no-install-recommends \
		ca-certificates \
		libpcre3 \
		libssl3 \
		perl-base \
		zlib1g && \
	rm -rf /var/lib/apt/lists/*

COPY --from=builder /usr/local/openresty /usr/local/openresty

WORKDIR /usr/local/openresty/nginx

EXPOSE 8080 8443 9090

CMD ["openresty", "-g", "daemon off;"]
