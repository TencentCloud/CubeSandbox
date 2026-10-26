#!/usr/bin/env bash

# //
# // Copyright (c) 2020 Ant Group
# //
# // SPDX-License-Identifier: Apache-2.0
# //

die() {
    cat <<EOF >&2
====================================================================
====                compile protocols failed                    ====

$1

====================================================================
EOF
    exit 1
}

show_succeed_msg() {
    echo "===================================================================="
    echo "====                                                            ===="
    echo "====                compile protocols succeed                   ===="
    echo "====                                                            ===="
    echo "===================================================================="
}

show_usage() {
    echo "===================================================================="
    echo ""
    echo "       USAGE: make PROTO_FILE=<xyz.proto> generate-protocols"
    echo ""
    echo "       Where PROTO_FILE may be:"
    echo "         all: will compile all protocol buffer files"
    echo ""
    echo "       Or compile individually by using the exact proto file:"

    # iterate over proto files
    for file in "$@"
    do
        echo "         $file"
    done

    echo ""
    echo "===================================================================="
}

generate_go_sources() {
    local cmd="protoc -I$GOPATH/src:$GOPATH/src/github.com/kata-containers/kata-containers/src/libs/protocols/protos \
--gogottrpc_out=plugins=ttrpc+fieldpath,\
import_path=github.com/kata-containers/kata-containers/src/runtime/virtcontainers/pkg/agent/protocols/grpc,\
\
Mgithub.com/kata-containers/kata-containers/src/libs/protocols/protos/csi.proto=github.com/kata-containers/kata-containers/src/runtime/virtcontainers/pkg/agent/protocols/grpc,\
\
Mgithub.com/kata-containers/kata-containers/src/libs/protocols/protos/types.proto=github.com/kata-containers/kata-containers/src/runtime/virtcontainers/pkg/agent/protocols,\
\
Mgithub.com/kata-containers/kata-containers/src/libs/protocols/protos/oci.proto=github.com/kata-containers/kata-containers/src/runtime/virtcontainers/pkg/agent/protocols/grpc,\
\
Mgogoproto/gogo.proto=github.com/gogo/protobuf/gogoproto,Mgoogle/protobuf/any.proto=github.com/gogo/protobuf/types,Mgoogle/protobuf/descriptor.proto=github.com/gogo/protobuf/protoc-gen-gogo/descriptor,Mgoogle/protobuf/duration.proto=github.com/gogo/protobuf/types,Mgoogle/protobuf/empty.proto=github.com/gogo/protobuf/types,Mgoogle/protobuf/field_mask.proto=github.com/gogo/protobuf/types,Mgoogle/protobuf/timestamp.proto=github.com/gogo/protobuf/types,Mgoogle/protobuf/wrappers.proto=github.com/gogo/protobuf/types,Mgoogle/rpc/status.proto=github.com/gogo/googleapis/google/rpc\
:$GOPATH/src \
$GOPATH/src/github.com/kata-containers/kata-containers/src/libs/protocols/protos/$1"

    echo $cmd
    $cmd
    [ $? -eq 0 ] || die "Failed to generate golang file from $1"
}

if [ "$(basename $(pwd))" != "agent" ]; then
	die "Please go to root directory of agent before execute this shell"
fi

# Protocol buffer files required to generate golang/rust bindings.
proto_files_list=(agent.proto csi.proto health.proto oci.proto types.proto)

if [ "$1" = "" ]; then
    show_usage "${proto_files_list[@]}"
    exit 1
fi;

# pre-requirement check
which protoc
[ $? -eq 0 ] || die "Please install protoc from github.com/protocolbuffers/protobuf"

which protoc-gen-gogottrpc
[ $? -eq 0 ] || die "Please install protoc-gen-gogottrpc from https://github.com/containerd/ttrpc"

# do generate work
target=$1

# compile all proto files
if [ "$target" = "all" ]; then
    # compile all proto files
    for f in ${proto_files_list[@]}; do
        echo -e "\n   [golang] compiling ${f} ..."
        generate_go_sources $f
        echo -e "   [golang] ${f} compiled\n"
    done
else
    # compile individual proto file
    for f in ${proto_files_list[@]}; do
        if [ "$target" = "$f" ]; then
            echo -e "\n   [golang] compiling ${target} ..."
            generate_go_sources $target
            echo -e "   [golang] ${target} compiled\n"
        fi
    done
fi;

# if have no errors, compilation will succeed
show_succeed_msg
