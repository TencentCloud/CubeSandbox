FROM ubuntu:22.04

ARG DEBIAN_FRONTEND=noninteractive
ARG KUBECTL_CHANNEL=v1.30

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      e2fsprogs \
      gnupg \
      iproute2 \
      kmod \
      mount \
      tini \
      util-linux \
      xfsprogs \
    && curl -fsSL "https://pkgs.k8s.io/core:/stable:/${KUBECTL_CHANNEL}/deb/Release.key" \
      | gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg \
    && echo "deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/${KUBECTL_CHANNEL}/deb/ /" \
      > /etc/apt/sources.list.d/kubernetes.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends kubectl \
    && rm -rf /var/lib/apt/lists/*

COPY scripts/node-prep-lib.sh /scripts/node-prep-lib.sh
COPY scripts/cube-node-init.sh /scripts/cube-node-init.sh
COPY scripts/wait-pvm-host.sh /scripts/wait-pvm-host.sh

RUN chmod +x /scripts/cube-node-init.sh /scripts/wait-pvm-host.sh \
    && chmod 644 /scripts/node-prep-lib.sh

ENTRYPOINT ["/usr/bin/tini", "-s", "--", "/scripts/cube-node-init.sh"]
