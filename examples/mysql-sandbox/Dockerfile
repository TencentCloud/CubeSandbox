# syntax=docker/dockerfile:1.7
#
# Demo: MySQL client sandbox for Cube Sandbox
#
# This image provides a MySQL client environment that can be used to:
# - Connect to MySQL databases from an isolated sandbox
# - Execute SQL queries with network isolation policies
# - Demonstrate snapshot/rollback capabilities
#
# Build:
#   docker build -t cubesandbox-mysql-sandbox:latest examples/mysql-sandbox
#
# Run locally:
#   docker run --rm -d -p 49983:49983 \
#     --name cube-mysql-sandbox cubesandbox-mysql-sandbox:latest
#   docker exec cube-mysql-sandbox mysql --version

ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive

# Install MySQL client and common database tools
RUN apt-get update && apt-get install -y --no-install-recommends \
    mysql-client \
    curl \
    wget \
    jq \
    && rm -rf /var/lib/apt/lists/*

# Create working directory for SQL scripts
RUN mkdir -p /srv/sql && chmod 755 /srv/sql

# Health check script that validates MySQL client is available
RUN printf '#!/bin/bash\nmysql --version\n' > /health-mysql.sh && chmod +x /health-mysql.sh

EXPOSE 49983

# cube-entrypoint.sh (inherited from the base image) will:
#   1. Launch envd on ${ENVD_PORT:-49983} in the background.
#   2. exec the CMD below as the foreground process.
CMD ["bash"]
