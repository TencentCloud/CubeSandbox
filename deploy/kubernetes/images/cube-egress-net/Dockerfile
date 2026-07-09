FROM ubuntu:22.04

ARG DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
      ca-certificates \
      iproute2 \
      iptables \
      kmod \
      procps \
      tini \
    && rm -rf /var/lib/apt/lists/*

COPY scripts/cube-proxy-iptables-init.sh /usr/local/bin/cube-proxy-iptables-init.sh
COPY scripts/cube-egress-net-entrypoint.sh /usr/local/bin/cube-egress-net-entrypoint.sh

RUN chmod +x \
      /usr/local/bin/cube-proxy-iptables-init.sh \
      /usr/local/bin/cube-egress-net-entrypoint.sh

ENTRYPOINT ["/usr/bin/tini", "-s", "--", "/usr/local/bin/cube-egress-net-entrypoint.sh"]
