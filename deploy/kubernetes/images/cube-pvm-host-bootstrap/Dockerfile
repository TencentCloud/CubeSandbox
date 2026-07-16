FROM ubuntu:24.04

ARG DEBIAN_FRONTEND=noninteractive
ARG KUBECTL_CHANNEL=v1.30

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      gnupg \
      kmod \
      rpm \
      tini \
      util-linux \
    && curl -fsSL "https://pkgs.k8s.io/core:/stable:/${KUBECTL_CHANNEL}/deb/Release.key" \
      | gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg \
    && echo "deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/${KUBECTL_CHANNEL}/deb/ /" \
      > /etc/apt/sources.list.d/kubernetes.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends kubectl \
    && rm -rf /var/lib/apt/lists/*

COPY scripts/node-prep-lib.sh /scripts/node-prep-lib.sh
COPY scripts/pvm-host-bootstrap.sh /scripts/pvm-host-bootstrap.sh

# Optional kernel packages. Put files under build context artifacts/ to build a fully self-contained image.
COPY artifacts/ /artifacts/

RUN chmod +x /scripts/pvm-host-bootstrap.sh \
    && chmod 644 /scripts/node-prep-lib.sh

ENTRYPOINT ["/usr/bin/tini", "-s", "--", "/scripts/pvm-host-bootstrap.sh"]
