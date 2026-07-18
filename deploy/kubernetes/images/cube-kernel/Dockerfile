FROM ubuntu:22.04

ARG DEBIAN_FRONTEND=noninteractive
ENV IMAGE_ROOT=/opt/cube-image
ENV TOOLBOX_ROOT=/usr/local/services/cubetoolbox
ENV CUBE_COMPONENT=cube-kernel
ENV CUBE_ROLE=install

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
      ca-certificates \
      tini \
      util-linux \
    && rm -rf /var/lib/apt/lists/*

COPY package/cube-kernel-scf ${IMAGE_ROOT}/cube-kernel-scf
COPY scripts/component-entrypoint.sh /usr/local/bin/component-entrypoint.sh

# Prefer digest-bearing identities when build args supply them; fall back to image tag.
ARG CUBE_VERSION=unknown
ARG CUBE_KERNEL_BM_VERSION=
ARG CUBE_KERNEL_PVM_VERSION=
RUN BM="${CUBE_KERNEL_BM_VERSION:-${CUBE_VERSION}}" \
    && PVM="${CUBE_KERNEL_PVM_VERSION:-${CUBE_VERSION}}" \
    && printf '{"schema_version":1,"variants":{"bm":{"version":"%s","tag":"%s"},"pvm":{"version":"%s","tag":"%s"}}}\n' \
      "${BM}" "${BM}" "${PVM}" "${PVM}" > ${IMAGE_ROOT}/cube-kernel-scf/version.json \
    && chmod +x /usr/local/bin/component-entrypoint.sh

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/component-entrypoint.sh"]
