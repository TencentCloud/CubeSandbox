# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# pkgs/proto — the single source of truth for shared gRPC contracts between
# CubeMaster, Cubelet and examples. Generated .pb.go files are committed, so
# consumers do not need protoc to build.
#
# Toolchain is pinned in the builder image (docker/Dockerfile.builder):
#   protoc v28.3 + protoc-gen-go v1.36.11 + protoc-gen-go-grpc v1.6.1

PROTO_PKGS = services/cubebox services/errorcode services/images \
             services/snapshot services/volumeplugin types

PROTOC_INCLUDE_FLAGS=-I=. -I=./third_party
PROTOC_GO_FLAGS=--go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative

.PHONY: proto doc test fmt clean

check-proto-tools:
	@command -v protoc >/dev/null 2>&1 || { echo "error: protoc not installed"; exit 1; }
	@command -v protoc-gen-go >/dev/null 2>&1 || { echo "error: protoc-gen-go not installed"; exit 1; }
	@command -v protoc-gen-go-grpc >/dev/null 2>&1 || { echo "error: protoc-gen-go-grpc not installed"; exit 1; }

proto: check-proto-tools
	@for pkg in $(PROTO_PKGS); do \
		echo $$pkg; \
		protoc $(PROTOC_INCLUDE_FLAGS) $(PROTOC_GO_FLAGS) $$pkg/v1/*.proto; \
	done

doc: check-proto-tools
	@if command -v protoc-gen-doc >/dev/null 2>&1; then \
		mkdir -p doc; \
		echo "Generate proto doc"; \
		protoc $(PROTOC_INCLUDE_FLAGS) --doc_out=./doc --doc_opt=markdown,api.md \
			services/cubebox/v1/cubebox.proto \
			services/errorcode/v1/errorcode.proto \
			services/images/v1/images.proto \
			services/snapshot/v1/snapshot.proto \
			services/volumeplugin/v1/volumeplugin.proto \
			types/v1/images.proto \
			types/v1/descriptor.proto; \
	else \
		echo "Skip proto doc (missing protoc-gen-doc)"; \
	fi

test:
	go test ./...

fmt:
	go fmt ./...
