# 基础镜像：使用 CubeSandbox 官方提供的沙箱代码运行环境
FROM cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest

# 配置 npm 全局安装路径为 /usr/local，确保 root 用户能正常使用
# 必须在 npm install -g 之前设置，以影响全局安装路径解析
ENV NPM_CONFIG_PREFIX=/usr/local

# 安装 Node.js 22.x（Claude Code 要求 Node >= 18；22.x 为当前 LTS，与运行时脚本一致）
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y nodejs \
    && rm -rf /var/lib/apt/lists/*  # 清理 apt 缓存以减小镜像体积

# 全局安装 Claude Code CLI 工具
RUN npm install -g @anthropic-ai/claude-code

# 验证 Node.js、npm 和 Claude Code 的安装版本
RUN node --version && npm --version && claude --version
