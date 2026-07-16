FROM ubuntu:22.04

ARG DEBIAN_FRONTEND=noninteractive

# Minimal image for Big Pod wait-node-prep sidecar (Kruise priority) and
# bootstrap write-ready/hold. Bumping wait image on Big Pod may InPlace; do not
# change wait env/mounts as routine upgrades.

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
      ca-certificates \
      tini \
    && rm -rf /var/lib/apt/lists/*

COPY scripts/node-prep-lib.sh /scripts/node-prep-lib.sh
COPY scripts/wait-node-prep.sh /scripts/wait-node-prep.sh
COPY scripts/write-node-prep-ready.sh /scripts/write-node-prep-ready.sh

RUN chmod +x /scripts/wait-node-prep.sh /scripts/write-node-prep-ready.sh \
    && chmod 644 /scripts/node-prep-lib.sh

ENTRYPOINT ["/usr/bin/tini", "-s", "--", "/scripts/wait-node-prep.sh"]
