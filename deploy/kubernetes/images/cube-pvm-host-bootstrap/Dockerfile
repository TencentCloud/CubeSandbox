FROM bitnami/kubectl:1.30 AS kubectl

FROM ubuntu:20.04

ARG DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      kmod \
      rpm \
      tini \
      util-linux \
    && rm -rf /var/lib/apt/lists/*

COPY --from=kubectl /opt/bitnami/kubectl/bin/kubectl /usr/local/bin/kubectl
COPY scripts/pvm-host-bootstrap.sh /scripts/pvm-host-bootstrap.sh

# Optional kernel packages. Put files under build context artifacts/ to build a fully self-contained image.
COPY artifacts/ /artifacts/

RUN chmod +x \
      /usr/local/bin/kubectl \
      /scripts/pvm-host-bootstrap.sh

ENTRYPOINT ["/usr/bin/tini", "-s", "--", "/scripts/pvm-host-bootstrap.sh"]
