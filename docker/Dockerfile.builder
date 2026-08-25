# syntax=docker/dockerfile:1.7

FROM ubuntu:20.04

ARG DEBIAN_FRONTEND=noninteractive
# Base URL of a China-reachable Ubuntu apt mirror (set via `make builder-image
# MIRROR=cn`). When set, amd64 packages resolve under ${APT_MIRROR_BASE}/ubuntu and
# arm64 under ${APT_MIRROR_BASE}/ubuntu-ports; empty leaves the image default
# (archive.ubuntu.com on amd64, ports.ubuntu.com on arm64). The rewrite is also
# skipped when GITHUB_ACTIONS=true so CI always builds against upstream.
ARG APT_MIRROR_BASE=
ARG GO_VERSION=1.25.7
ARG PROTOC_VERSION=28.3
ARG LIBSECCOMP_VERSION=2.5.5
ARG RUST_TOOLCHAIN_DEFAULT=1.89
ARG RUST_TOOLCHAIN_HYPERVISOR=1.77.2
ARG RUST_TOOLCHAIN_E2BAPI=1.85
ARG RUST_TOOLCHAIN_AGENT=1.89
ARG GITHUB_ACTIONS=false
ARG RUSTUP_DIST_SERVER=https://rsproxy.cn
ARG RUSTUP_UPDATE_ROOT=https://rsproxy.cn/rustup
# Base URL of a China-reachable LLVM apt mirror (e.g. set via `make builder-image
# MIRROR=cn`). When set, the llvm.sh installer script and the clang-14 apt packages
# are sourced from this mirror; empty uses upstream apt.llvm.org. The LLVM GPG
# signing key is always fetched from apt.llvm.org regardless of this value.
ARG LLVM_MIRROR_BASE=
ARG TARGETARCH

ENV LANG=C.UTF-8 \
    LC_ALL=C.UTF-8 \
    GOPATH=/go \
    RUSTUP_HOME=/usr/local/rustup \
    CARGO_HOME=/usr/local/cargo \
    PATH=/usr/local/go/bin:/go/bin:/usr/local/cargo/bin:${PATH} \
    CARGO_NET_GIT_FETCH_WITH_CLI=true \
    OPENSSL_INCLUDE_DIR=/usr/include \
    X86_64_UNKNOWN_LINUX_GNU_OPENSSL_LIB_DIR=/usr/lib/x86_64-linux-gnu \
    X86_64_UNKNOWN_LINUX_MUSL_OPENSSL_LIB_DIR=/usr/lib/x86_64-linux-gnu \
    AARCH64_UNKNOWN_LINUX_GNU_OPENSSL_LIB_DIR=/usr/lib/aarch64-linux-gnu \
    AARCH64_UNKNOWN_LINUX_MUSL_OPENSSL_LIB_DIR=/usr/lib/aarch64-linux-gnu \
    LIBSECCOMP_LINK_TYPE=static \
    LIBSECCOMP_LIB_PATH=/usr/local/lib64/libseccomp/lib

RUN set -eux; \
    TARGETARCH="${TARGETARCH:-$(dpkg --print-architecture)}"; \
    case "${TARGETARCH}" in \
      amd64) PROTOC_ARCH=x86_64;; \
      arm64) PROTOC_ARCH=aarch_64;; \
      *)     PROTOC_ARCH=$(uname -m | sed 's/^aarch64$/aarch_64/');; \
    esac; \
    { \
      echo "TARGETARCH=${TARGETARCH}"; \
      echo "TARGET_UNAME_ARCH=$(uname -m)"; \
      echo "PROTOC_ARCH=${PROTOC_ARCH}"; \
    } > /etc/buildenv

RUN apt-get update -o Acquire::Retries=3 \
    && apt install -y ca-certificates --no-install-recommends

RUN set -eux; \
    if [ "${GITHUB_ACTIONS}" != "true" ] && [ -n "${APT_MIRROR_BASE}" ]; then \
        case "${APT_MIRROR_BASE}" in \
            http://*|https://*) ;; \
            *) echo "APT_MIRROR_BASE must be an http(s) URL: ${APT_MIRROR_BASE}" >&2; exit 1;; \
        esac; \
        case "${APT_MIRROR_BASE}" in \
            *[\$\`\"\;\!\&\|\\]*) echo "APT_MIRROR_BASE contains characters unsafe for sed: ${APT_MIRROR_BASE}" >&2; exit 1;; \
        esac; \
        sed -i "s|http://archive.ubuntu.com/ubuntu|${APT_MIRROR_BASE}/ubuntu|g; \
                s|http://security.ubuntu.com/ubuntu|${APT_MIRROR_BASE}/ubuntu|g; \
                s|http://ports.ubuntu.com/ubuntu-ports|${APT_MIRROR_BASE}/ubuntu-ports|g" \
            /etc/apt/sources.list; \
    fi

RUN . /etc/buildenv \
    && apt-get update -o Acquire::Retries=3 \
    && apt-get install -y --no-install-recommends \
        bash \
        bc \
        binutils-dev \
        build-essential \
        ca-certificates \
        clang \
        cmake \
        cpio \
        curl \
        dmsetup \
        dnsmasq \
        dosfstools \
        file \
        flex \
        bison \
        gperf \
        git \
        git-lfs \
        jq \
        libaio-dev \
        libcap-dev \
        libcap-ng-dev \
        libcmocka-dev \
        libcunit1-dev \
        libdevmapper-dev \
        libelf-dev \
        libbpf-dev \
        libfuse3-dev \
        libglib2.0-dev \
        libiscsi-dev \
        libjson-c-dev \
        libkeyutils-dev \
        libncurses5-dev \
        libncursesw5-dev \
        libnuma-dev \
        libpixman-1-dev \
        libseccomp-dev \
        libssl-dev \
        libtool \
        llvm \
        make \
        meson \
        mtools \
        musl-tools \
        docker.io \
        nasm \
        ninja-build \
        ntfs-3g \
        patchelf \
        pkg-config \
        procps \
        python-is-python3 \
        python3 \
        python3-dev \
        python3-distutils \
        python3-pip \
        python3-pyelftools \
        python3-setuptools \
        python3-venv \
        python3.9 \
        python3.9-distutils \
        python3.9-venv \
        qemu-utils \
        socat \
        sudo \
        unzip \
        uuid-dev \
        wget \
        xz-utils \
        zip \
        zlib1g-dev \
        autoconf \
        automake \
        help2man \
        gnupg \
        lsb-release \
        software-properties-common \
    && if [ "${TARGETARCH}" = "amd64" ]; then \
       apt-get install -y --no-install-recommends gcc-multilib; \
       apt-get install -y gcc-aarch64-linux-gnu; \
    else \
       apt-get install -y --no-install-recommends; \
       apt-get install -y gcc-x86-64-linux-gnu; \
    fi \
    && rm -rf /var/lib/apt/lists/*

# Python build-deps for CubeS3lvol's SPDK/DPDK (populated by setup_dep.sh).
# The pinned SPDK needs python >= 3.9 (genrpc.py uses
# argparse.BooleanOptionalAction) and DPDK needs meson >= 0.57.2, neither of
# which ubuntu 20.04's stock python3.8 / apt meson (0.53.2) provides. Install
# python3.9 and keep its toolchain in a venv that setup_dep.sh puts on its own
# PATH, so the shared builder's /usr/bin/python3 and apt meson stay untouched
# for the Go/Rust/kernel tracks.
RUN /usr/bin/python3.9 -m venv /opt/s3lvol-tools \
    && /opt/s3lvol-tools/bin/pip install --no-cache-dir \
        meson==1.10.0 \
        ninja==1.11.1 \
        jinja2==3.1.4 \
        tabulate==0.9.0 \
        pyelftools==0.31

# Install clang-14. With LLVM_MIRROR_BASE set, fetch llvm.sh from the mirror and
# pass `-m` so the apt repo/packages resolve to the mirror too (the upstream
# script otherwise hardcodes BASE_URL=apt.llvm.org). Note: llvm.sh always fetches
# the GPG signing key from apt.llvm.org (that URL is hardcoded and the mirror does
# not serve the key), so apt.llvm.org must remain reachable for the key request.
RUN set -eux; \
    if [ -n "${LLVM_MIRROR_BASE}" ]; then \
        script_url="${LLVM_MIRROR_BASE}/llvm.sh"; set -- 14 -m "${LLVM_MIRROR_BASE}"; \
    else \
        script_url="https://apt.llvm.org/llvm.sh"; set -- 14; \
    fi; \
    wget -O /tmp/llvm.sh "${script_url}"; bash /tmp/llvm.sh "$@"; rm -f /tmp/llvm.sh; \
    apt-get install -y llvm-14 \
    && rm -rf /var/lib/apt/lists/* && clang-14 --version && llvm-strip-14 --version \
    && update-alternatives --install /usr/bin/clang clang /usr/bin/clang-14 100 \
    && update-alternatives --install /usr/bin/clang++ clang++ /usr/bin/clang++-14 100 \
    && if [ -x /usr/bin/llvm-strip-14 ] && [ ! -e /usr/local/bin/llvm-strip ]; then ln -s /usr/bin/llvm-strip-14 /usr/local/bin/llvm-strip; fi \
    && if [ ! -e /usr/bin/musl-g++ ]; then ln -s /usr/bin/g++ /usr/bin/musl-g++; fi

RUN . /etc/buildenv \
    && curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz" -o /tmp/go.tgz \
    && rm -rf /usr/local/go \
    && tar -C /usr/local -xzf /tmp/go.tgz \
    && rm -f /tmp/go.tgz

RUN . /etc/buildenv \
    && wget -q "https://github.com/protocolbuffers/protobuf/releases/download/v${PROTOC_VERSION}/protoc-${PROTOC_VERSION}-linux-${PROTOC_ARCH}.zip" -O /tmp/protoc.zip \
    && unzip -q /tmp/protoc.zip -d /tmp/protoc \
    && install -m 0755 /tmp/protoc/bin/protoc /usr/local/bin/protoc \
    && cp -r /tmp/protoc/include/* /usr/local/include/ \
    && rm -rf /tmp/protoc /tmp/protoc.zip

RUN go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11 \
    && go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.1 \
    && go install github.com/pseudomuto/protoc-gen-doc/cmd/protoc-gen-doc@v1.5.1

RUN curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs \
    | sh -s -- -y --profile minimal --default-toolchain none

ENV RUSTUP_DIST_SERVER="${RUSTUP_DIST_SERVER}"
ENV RUSTUP_UPDATE_ROOT="${RUSTUP_UPDATE_ROOT}"

RUN set -eux; \
    . /etc/buildenv \
    && for toolchain in "${RUST_TOOLCHAIN_HYPERVISOR}" "${RUST_TOOLCHAIN_E2BAPI}" "${RUST_TOOLCHAIN_AGENT}"; do \
        rustup toolchain install "${toolchain}" --profile minimal; \
        rustup component add rust-src clippy rustfmt rust-analyzer llvm-tools-preview --toolchain "${toolchain}"; \
        rustup target add ${TARGET_UNAME_ARCH}-unknown-linux-musl --toolchain "${toolchain}"; \
    done; \
    rustup default "${RUST_TOOLCHAIN_DEFAULT}"

RUN mkdir -p "${CARGO_HOME}" /root/.cargo \
    && printf '[registries.crates-io]\nprotocol = "sparse"\n\n[net]\ngit-fetch-with-cli = true\n' > "${CARGO_HOME}/config.toml" \
    && ln -sf "${CARGO_HOME}/config.toml" /root/.cargo/config.toml \
    && ln -sf "${CARGO_HOME}/env" /root/.cargo/env

RUN . /etc/buildenv \
    && tmp_dir="$(mktemp -d)" \
    && wget -q "https://github.com/seccomp/libseccomp/releases/download/v${LIBSECCOMP_VERSION}/libseccomp-${LIBSECCOMP_VERSION}.tar.gz" -O "${tmp_dir}/libseccomp.tgz" \
    && tar -xzf "${tmp_dir}/libseccomp.tgz" -C "${tmp_dir}" --strip-components=1 \
    && cd "${tmp_dir}" \
    && CC=musl-gcc ./configure --host=${TARGET_UNAME_ARCH}-linux-musl CPPFLAGS="-I/usr/include/${TARGET_UNAME_ARCH}-linux-musl -idirafter /usr/include -idirafter /usr/include/${TARGET_UNAME_ARCH}-linux-gnu" CFLAGS="-O2 -I/usr/include/${TARGET_UNAME_ARCH}-linux-musl -idirafter /usr/include -idirafter /usr/include/${TARGET_UNAME_ARCH}-linux-gnu" --disable-shared --enable-static --prefix=/usr/local/lib64/libseccomp \
    && make -j"$(nproc)" \
    && make install \
    && rm -rf "${tmp_dir}"

RUN . /etc/buildenv \
    && openssl_dir=/usr/include/${TARGET_UNAME_ARCH}-linux-gnu/openssl \
    && if [ -n "${openssl_dir}" ] && [ -f "${openssl_dir}/opensslconf.h" ] && [ ! -f /usr/include/openssl/opensslconf.h ]; then \
        cp "${openssl_dir}/opensslconf.h" /usr/include/openssl/opensslconf.h; \
    fi

WORKDIR /workspace

CMD ["/bin/bash"]
