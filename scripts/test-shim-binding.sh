#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
fixture="$(mktemp)"
trap 'rm -f "$fixture"' EXIT
export CUBE_SHIM_BINDING_FIXTURE="$fixture"
(
    cd "$root/CubeMaster"
    go test -count=1 -gcflags=all=-l ./pkg/service/httpservice/cube -run '^TestMasterShimBindingCreateFlow$'
)
(
    cd "$root/Cubelet"
    go test -count=1 ./services/cubebox -run '^TestMasterShimBindingReachesRuntimePath$'
)
