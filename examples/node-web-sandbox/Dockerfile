# syntax=docker/dockerfile:1.7
#
# CubeSandbox Node.js web development template example.
#
# Build locally:
#   docker build -t cubesandbox-node-web:latest examples/node-web-sandbox
#
# Run locally:
#   docker run --rm -p 3000:3000 -p 49983:49983 cubesandbox-node-web:latest
#   curl http://127.0.0.1:3000/api/hello
#   curl -o /dev/null -w "%{http_code}\n" http://127.0.0.1:49983/health

ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16

FROM ${CUBE_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive
ARG NODE_MAJOR=20

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl gnupg python3 \
    && curl -fsSL "https://deb.nodesource.com/setup_${NODE_MAJOR}.x" | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /workspace/node-web-sandbox
COPY package.json server.js smoke_test.py ./
COPY start-node-web-sandbox.sh /usr/local/bin/start-node-web-sandbox.sh
RUN chmod +x /usr/local/bin/start-node-web-sandbox.sh

ENV HOST=0.0.0.0
ENV PORT=3000
ENV ENVD_PORT=49983
ENV NODE_ENV=production

EXPOSE 3000 49983

HEALTHCHECK --interval=10s --timeout=3s --retries=5 \
    CMD node -e "fetch('http://127.0.0.1:' + (process.env.PORT || 3000) + '/health').then(r => process.exit(r.ok ? 0 : 1)).catch(() => process.exit(1))"

# Start envd explicitly so the image also works when using CubeSandbox images
# that provide envd but do not define a Docker entrypoint.
ENTRYPOINT ["/usr/local/bin/start-node-web-sandbox.sh"]
CMD ["node", "server.js"]
