# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

.PHONY: build run test vet fmt clean docker

BINARY=cubeops
BIND=127.0.0.1:3010

build:
	go build -o bin/$(BINARY) ./cmd/cubeops

run: build
	JWT_SECRET=test-secret-dummy \
	DATABASE_URL=mysql://cube:cube_pass@127.0.0.1:3306/cube_mvp \
	CUBE_MASTER_ADDR=http://127.0.0.1:8089 \
	./bin/$(BINARY)

test:
	go test ./... -v -count=1

vet:
	go vet ./...

fmt:
	go fmt ./...
	@command -v goimports >/dev/null 2>&1 && goimports -w . || true

clean:
	rm -rf bin/

docker:
	docker build -t cube-ops:latest -f Dockerfile ..

tidy:
	go mod tidy
