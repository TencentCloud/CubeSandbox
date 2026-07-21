# Thin wrapper on the official Cube Sandbox code image.
# Purpose: give this example a buildable template path for #645, without
# adding a new language/runtime stack.
#
# Markers baked into the image:
#   - ENV TOOL_ALLOWLIST_SANDBOX=1
#   - /etc/cube-sandbox/tool-allowlist.txt (demo allowlist file)
#
# The allowlist file body must stay aligned with allowlist.DEFAULT_ALLOWED_BINARIES.
# Check with: python verify_local.py
#
# Build (from this directory):
#   docker build -t agent-tool-allowlist-sandbox:latest .
#
# Register as a Cube template (same flags as code-sandbox-quickstart):
#   cubemastercli tpl create-from-image \
#     --image <your-registry>/agent-tool-allowlist-sandbox:latest \
#     --writable-layer-size 1G \
#     --expose-port 49999 \
#     --expose-port 49983 \
#     --probe 49999
#
# Base image registries (same as examples/code-sandbox-quickstart/README.md):
#   CN:  cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest
#   INT: cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest

ARG SANDBOX_CODE_IMAGE=cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest
FROM ${SANDBOX_CODE_IMAGE}

ENV TOOL_ALLOWLIST_SANDBOX=1

# Demo allowlist file inside the guest (informational; host-side gate remains
# the enforcement point for this example — see README Limitations).
# Names are sorted to match allowlist_sync.allowlist_file_body().
RUN mkdir -p /etc/cube-sandbox \
    && printf '%s\n' \
        '# Agent tool argv allowlist (demo; host gate is authoritative)' \
        'cat' \
        'echo' \
        'head' \
        'ls' \
        'pwd' \
        'python3' \
        'sha256sum' \
        'uname' \
        'wc' \
    > /etc/cube-sandbox/tool-allowlist.txt

# Ports inherited from sandbox-code / quickstart convention.
EXPOSE 49999 49983
