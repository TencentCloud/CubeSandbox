module github.com/tencentcloud/CubeSandbox/OpsAgent

go 1.25.7

require (
	github.com/goccy/go-yaml v1.19.2
	github.com/tencentcloud/CubeSandbox/pkgs/CubeLog v0.1.1-0.20260113105508-a996703fa42f
)

require (
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/modern-go/concurrent v0.0.0-20180228061459-e0a39a4cb421 // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
)

replace github.com/tencentcloud/CubeSandbox/pkgs/CubeLog => ../pkgs/CubeLog
