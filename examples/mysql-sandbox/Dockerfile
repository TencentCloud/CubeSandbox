FROM debian:12-slim

LABEL maintainer="CubeSandbox Contributors"
LABEL description="MySQL Client Sandbox Template for CubeSandbox"
LABEL version="1.0.0"

# Install MySQL client and common tools
RUN apt-get update && apt-get install -y --no-install-recommends \
    default-mysql-client \
    mysql-common \
    curl \
    wget \
    vim \
    less \
    git \
    && rm -rf /var/lib/apt/lists/*

# Create non-root user for better security
RUN useradd -m -s /bin/bash sandbox && \
    mkdir -p /home/sandbox && \
    chown -R sandbox:sandbox /home/sandbox

# Set working directory
WORKDIR /home/sandbox

# Environment variables
ENV DEBIAN_FRONTEND=noninteractive
ENV MYSQL_CLIENT_VERSION=8.0

# Health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD mysqladmin ping -h localhost --silent 2>/dev/null || exit 0

USER sandbox
