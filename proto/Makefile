GO_BIN_DIR := $(shell if [ -n "$$(go env GOBIN)" ]; then printf "%s" "$$(go env GOBIN)"; else printf "%s/bin" "$$(go env GOPATH)"; fi)
PROTOBUF_INCLUDE_DIR := $(shell python3 -c 'import glob, os, subprocess; candidates = ["/usr/include", "/usr/local/include"]; gopath = subprocess.check_output(["go", "env", "GOPATH"], text=True).strip(); extras = glob.glob(os.path.join(gopath, "pkg/mod/github.com/google/gnostic-models@*/third_party")) if gopath else []; candidates.extend(extras); print(next((c for c in candidates if os.path.exists(os.path.join(c, "google/protobuf/empty.proto")) and os.path.exists(os.path.join(c, "google/protobuf/any.proto"))), ""))')
ROOT_DIR := $(shell cd .. && pwd)
PROTOC_INCLUDE_FLAGS=-I$(ROOT_DIR)
ifneq ($(strip $(PROTOBUF_INCLUDE_DIR)),)
PROTOC_INCLUDE_FLAGS += -I=$(PROTOBUF_INCLUDE_DIR)
endif
PROTOC_GO_FLAGS=--go_out=$(ROOT_DIR) --go_opt=paths=source_relative --go-grpc_out=$(ROOT_DIR) --go-grpc_opt=paths=source_relative
export PATH := $(GO_BIN_DIR):$(PATH)

PROTO=services/errorcode services/cubebox services/images types

.PHONY: proto check-proto-tools

check-proto-tools:
	@command -v protoc >/dev/null 2>&1 || { echo "error: protoc not installed"; exit 1; }
	@command -v protoc-gen-go >/dev/null 2>&1 || { echo "error: protoc-gen-go not installed"; exit 1; }
	@command -v protoc-gen-go-grpc >/dev/null 2>&1 || { echo "error: protoc-gen-go-grpc not installed"; exit 1; }
	@if [ -z "$(PROTOBUF_INCLUDE_DIR)" ]; then echo "error: protobuf include directory not found"; exit 1; fi

proto: check-proto-tools
	@printf '  %-8s %s\n' "Generate" "shared api proto"
	@( for pkg in ${PROTO}; do \
		echo $$pkg ;\
		protoc $(PROTOC_INCLUDE_FLAGS) $(PROTOC_GO_FLAGS) \
		$(ROOT_DIR)/proto/$$pkg/v1/*.proto ;\
	done)
