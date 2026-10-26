# Go SDK

CubeSandbox Go SDK 支持沙箱生命周期管理、代码执行、命令、PTY、文件操作、快照、克隆、回滚、持久卷和网络策略。

## 安装

```bash
go get github.com/tencentcloud/CubeSandbox/sdk/go
```

## 配置

```bash
export CUBE_API_URL=http://127.0.0.1:3000
export CUBE_TEMPLATE_ID=<your-template-id>

# 可选的远程数据面访问配置
export CUBE_PROXY_NODE_IP=<cubeproxy-node-ip>
export CUBE_PROXY_PORT_HTTP=80
export CUBE_PROXY_SCHEME=http
export CUBE_SANDBOX_DOMAIN=cube.app
```

`NewConfigFromEnv` 同时支持 `E2B_API_URL` 和 `E2B_API_KEY`；Cube 专用环境变量的优先级更高。

## 快速开始

```go
package main

import (
    "context"
    "fmt"

    cubesandbox "github.com/tencentcloud/CubeSandbox/sdk/go"
)

func main() {
    ctx := context.Background()
    client := cubesandbox.NewClient(cubesandbox.NewConfigFromEnv())
    defer client.Close()

    sandbox, err := client.Create(ctx, cubesandbox.CreateOptions{})
    if err != nil {
        panic(err)
    }
    defer sandbox.Kill(ctx)

    result, err := sandbox.RunCode(ctx, "x = 41\nx + 1", cubesandbox.RunCodeOptions{})
    if err != nil {
        panic(err)
    }
    fmt.Println(result.Text)
}
```

`Client.Close` 只释放本地空闲 HTTP 连接，不会销毁远程沙箱。使用 `Sandbox.Kill` 清理沙箱，或使用 `Sandbox.Pause` 保留沙箱以便之后重新连接。

## 主要能力

- 执行代码和 Shell 命令。
- 创建、重连、调整和控制交互式 PTY。
- 完整的文件系统操作和目录监听。
- 创建快照、回滚以及并发克隆。
- 管理持久卷和宿主机目录挂载。
- 暂停并重新连接沙箱。
- 配置节点调度、远程代理以及 L3/L4、L7 出网策略。

详细的方法说明和集成测试步骤请查看 [Go SDK README](https://github.com/TencentCloud/CubeSandbox/blob/master/sdk/go/README.md)。
