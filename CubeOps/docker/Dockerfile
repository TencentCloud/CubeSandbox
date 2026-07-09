# syntax=docker/dockerfile:1

# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

# ── Build stage ────────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /build

# Copy go.mod/go.sum first for better layer caching
COPY go.mod go.sum ./
COPY ../cubedb /build/../cubedb
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w" \
    -o /usr/local/bin/cubeops \
    ./cmd/cubeops

# ── Runtime stage ──────────────────────────────────────────────────────────
FROM alpine:3.20

RUN apk add --no-cache ca-certificates docker-cli tzdata

COPY --from=builder /usr/local/bin/cubeops /usr/local/bin/cubeops

EXPOSE 3010

ENTRYPOINT ["cubeops"]
