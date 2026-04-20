# syntax=docker/dockerfile:1.7
#
# Demo: a Cube-ready sandbox image that runs nginx on :80 while the
# cubesandbox-base entrypoint keeps envd running on :49983 in the
# background. Good for smoke-testing `cubemastercli tpl create-from-image`.
#
# Build:
#   docker build -t cubesandbox-demo-nginx:latest examples/cubesandbox-base-nginx
#
# Run locally:
#   docker run --rm -d -p 8080:80 -p 49983:49983 \
#     --name cube-demo-nginx cubesandbox-demo-nginx:latest
#   curl http://127.0.0.1:8080/
#   curl -o /dev/null -w "%{http_code}\n" http://127.0.0.1:49983/health  # => 204

ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
    && apt-get install -y --no-install-recommends nginx \
    && rm -rf /var/lib/apt/lists/*

# Tiny landing page so you can eyeball that nginx really served the request.
RUN printf '%s\n' \
        '<!doctype html>' \
        '<title>cubesandbox-demo-nginx</title>' \
        '<h1>Hello from nginx inside a CubeSandbox base image</h1>' \
        '<p>envd is running on :49983, nginx on :80.</p>' \
    > /var/www/html/index.html

EXPOSE 80 49983

# cube-entrypoint.sh (inherited from the base image) will:
#   1. Launch envd on ${ENVD_PORT:-49983} in the background.
#   2. exec the CMD below as the foreground process, so nginx owns PID 1
#      logs and receives SIGTERM on `docker stop`.
CMD ["nginx", "-g", "daemon off;"]
