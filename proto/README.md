# proto — Shared Protobuf Definitions

This Go module (`github.com/tencentcloud/CubeSandbox/proto`) contains the
canonical protobuf service and type definitions shared between **CubeMaster**
and **Cubelet**.

## Contents

| Package | Description |
|---|---|
| `services/cubebox/v1` | Sandbox lifecycle RPCs (create, destroy, exec, …) |
| `services/errorcode/v1` | Shared error code enum |
| `services/images/v1` | Image pull / push / list RPCs |
| `types/v1` | Common types (ImageSpec, AuthConfig, …) |

## Regenerating Go Code

Prerequisites: `protoc`, `protoc-gen-go`, `protoc-gen-go-grpc`.

```bash
cd proto/
make proto
```

This regenerates all `*.pb.go` and `*_grpc.pb.go` files in place.

## Consuming This Module

Both CubeMaster and Cubelet reference this module via a `replace` directive
for local development:

```go
// In CubeMaster/go.mod or Cubelet/go.mod
replace github.com/tencentcloud/CubeSandbox/proto => ../proto
```

Import paths follow the pattern:

```go
import cubebox "github.com/tencentcloud/CubeSandbox/proto/services/cubebox/v1"
```
