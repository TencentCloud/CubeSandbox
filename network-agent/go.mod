module github.com/tencentcloud/CubeSandbox/network-agent

go 1.24.0

require (
	github.com/cilium/ebpf v0.17.3
	github.com/pelletier/go-toml/v2 v2.2.4
	github.com/tencentcloud/CubeSandbox/CubeNet/cubevs v0.0.0
	github.com/tencentcloud/CubeSandbox/cubelog v0.1.0
	github.com/vishvananda/netlink v1.1.0
	golang.org/x/sys v0.39.0
	google.golang.org/grpc v1.79.2
	google.golang.org/protobuf v1.36.10
)

require (
	github.com/florianl/go-tc v0.4.1 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/josharian/native v1.1.0 // indirect
	github.com/mdlayher/netlink v1.7.2 // indirect
	github.com/mdlayher/socket v0.4.1 // indirect
	github.com/vishvananda/netns v0.0.0-20191106174202-0a2b9b5464df // indirect
	golang.org/x/net v0.48.0 // indirect
	golang.org/x/sync v0.19.0 // indirect
	golang.org/x/text v0.32.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20251202230838-ff82c1b0f217 // indirect
)

replace github.com/tencentcloud/CubeSandbox/CubeNet/cubevs => ../CubeNet/cubevs

replace github.com/tencentcloud/CubeSandbox/cubelog => ../cubelog
