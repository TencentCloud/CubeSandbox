# syntax=docker/dockerfile:1.7
#
# A Go toolchain sandbox template: the official Go distribution stacked on top
# of `cubesandbox-base`, so `go build` / `go test` / `go run` work through the
# SDK's command API while the inherited entrypoint keeps envd on :49983.
#
# Build:
#   docker build -t <your-registry>/go-dev-sandbox:latest examples/go-dev-sandbox
#
# Run locally (sanity check before pushing):
#   docker run --rm -d -p 49983:49983 --name go-dev go-dev-sandbox:latest
#   curl -o /dev/null -w "%{http_code}\n" http://127.0.0.1:49983/health  # => 204
#   docker exec go-dev go version
#   docker rm -f go-dev

ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG GO_VERSION=1.24.9
ARG DEBIAN_FRONTEND=noninteractive
# Provided by BuildKit; used to pick the matching Go distribution tarball.
ARG TARGETARCH

# git is needed for `go get` / module resolution against VCS-backed modules.
RUN apt-get update \
    && apt-get install -y --no-install-recommends git \
    && rm -rf /var/lib/apt/lists/*

# BuildKit populates TARGETARCH; the legacy builder does not. Falling back to a
# hard-coded "amd64" would silently fetch the wrong tarball on an arm64 host, so
# derive it from the image itself instead.
RUN goarch="${TARGETARCH:-$(dpkg --print-architecture)}" \
    && curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${goarch}.tar.gz" \
        -o /tmp/go.tar.gz \
    && tar -C /usr/local -xzf /tmp/go.tar.gz \
    && rm /tmp/go.tar.gz

ENV GOROOT=/usr/local/go \
    GOPATH=/home/user/go \
    GOCACHE=/home/user/.cache/go-build \
    GOMODCACHE=/home/user/go/pkg/mod \
    PATH=/usr/local/go/bin:/home/user/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    CGO_ENABLED=0 \
    GOFLAGS=-buildvcs=false

# envd may execute commands as either `root` (the SDK default) or `user`
# (uid 1000, the base image default), so the build cache and the workspace
# have to be writable by both.
# envd does not guarantee that the image's PATH reaches every spawned process,
# so expose `go` and `gofmt` at a location that is always on PATH.
RUN ln -sf /usr/local/go/bin/go /usr/local/bin/go \
    && ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt

RUN mkdir -p "${GOPATH}" "${GOCACHE}" /workspace \
    && chown -R user:user "${GOPATH}" "${GOCACHE}" /workspace \
    && chmod -R 0777 "${GOPATH}" "${GOCACHE}" /workspace \
    && go version

WORKDIR /workspace

EXPOSE 49983
