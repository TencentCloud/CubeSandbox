FROM ghcr.io/tencentcloud/cubesandbox-base:2026.16 AS cubesandbox-base

FROM node:24-bookworm-slim

COPY --from=cubesandbox-base /usr/bin/envd /usr/bin/envd
COPY --from=cubesandbox-base /usr/bin/tini /usr/bin/tini
COPY --from=cubesandbox-base /usr/local/bin/cube-entrypoint.sh /usr/local/bin/cube-entrypoint.sh

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /workspace/node-fastify-web

COPY package*.json tsconfig.json ./
COPY src ./src
RUN npm ci \
    && npm run build \
    && npm prune --omit=dev

EXPOSE 49983 3000

ENTRYPOINT ["/usr/bin/tini", "-g", "--", "/usr/local/bin/cube-entrypoint.sh"]
CMD ["node", "dist/main.js"]
