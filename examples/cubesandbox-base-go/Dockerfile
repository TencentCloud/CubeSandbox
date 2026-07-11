# syntax=docker/dockerfile:1.7
#
# Demo: a Cube-ready sandbox image that runs a tiny Go HTTP server on :8080
# while the cubesandbox-base entrypoint keeps envd running on :49983 in the
# background. A good starting point for Go workloads in CubeSandbox.
#
# Build:
#   docker build -t cubesandbox-demo-go:latest examples/cubesandbox-base-go
#
# Run locally:
#   docker run --rm -d -p 8080:8080 -p 49983:49983 \
#     --name cube-demo-go cubesandbox-demo-go:latest
#   curl http://127.0.0.1:8080/
#   curl -o /dev/null -w "%{http_code}\n" http://127.0.0.1:49983/health  # => 204

ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive
ARG GO_VERSION=1.23.4

# Install the Go toolchain from the official tarball — newer than Ubuntu
# 22.04's apt golang (1.18) and self-contained under /usr/local/go.
RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" -o /tmp/go.tgz \
    && tar -C /usr/local -xzf /tmp/go.tgz \
    && rm /tmp/go.tgz \
    && /usr/local/go/bin/go version

ENV GOROOT=/usr/local/go \
    GOPATH=/go \
    PATH="/usr/local/go/bin:/go/bin:${PATH}"

WORKDIR /app
COPY go.mod /app/go.mod
COPY main.go /app/main.go
COPY main_test.go /app/main_test.go

# Run unit tests (httptest-based, no network needed) before building.
RUN CGO_ENABLED=0 go test -v .

# Build a static-ish binary; CGO_ENABLED=0 avoids libc linkage surprises.
RUN CGO_ENABLED=0 go build -o /app/helloserver main.go

EXPOSE 8080 49983

# cube-entrypoint.sh (inherited from the base image) will:
#   1. Launch envd on ${ENVD_PORT:-49983} in the background.
#   2. exec the CMD below as the foreground process, so the Go server owns
#      PID 1's child and receives SIGTERM on `docker stop`.
CMD ["/app/helloserver"]
