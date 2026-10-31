# syntax=docker/dockerfile:1.7

ARG SANDBOX_CODE_IMAGE=cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest
FROM ${SANDBOX_CODE_IMAGE}

COPY lightweight-code-interpreter/server.py /opt/lightweight-code-interpreter/server.py
COPY lightweight-code-interpreter/launch.py /opt/lightweight-code-interpreter/launch.py

ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["python", "/opt/lightweight-code-interpreter/launch.py"]
