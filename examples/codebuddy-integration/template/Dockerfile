FROM cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest

# Install Node.js 22 LTS
# Try official source first, fall back to China mirror for users behind GFW.
# Uses .tar.gz because the base image may not have xz-utils installed.
ENV NODE_VERSION=22.11.0
RUN (curl -fsSL --connect-timeout 10 -o /tmp/node.tar.gz https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.gz || \
     curl -fsSL -o /tmp/node.tar.gz https://npmmirror.com/mirrors/node/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.gz) && \
    tar -xzf /tmp/node.tar.gz -C /usr/local --strip-components=1 && \
    rm /tmp/node.tar.gz && \
    node --version && npm --version

# Install CodeBuddy CLI globally
# Try default npm registry first, fall back to China mirror.
RUN npm install -g @tencent-ai/codebuddy-code@latest || \
    npm install -g @tencent-ai/codebuddy-code@latest \
        --registry=https://registry.npmmirror.com

# Create workspace directory
RUN mkdir -p /workspace
WORKDIR /workspace

# envd is the existing ENTRYPOINT from sandbox-code base image
# CodeBuddy runs on demand via sb.commands.run()
