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
- 完整的文件系统操作、流式文件上传与下载，以及目录监听。
- 创建快照、回滚以及并发克隆。
- 管理持久卷和宿主机目录挂载。
- 暂停并重新连接沙箱。
- 配置节点调度、远程代理以及 L3/L4、L7 出网策略。

详细的方法说明和集成测试步骤请查看 [Go SDK README](https://github.com/TencentCloud/CubeSandbox/blob/master/sdk/go/README.md)。

## 流式文件上传

`Files.WriteFrom(ctx, path, src io.Reader)` 从 reader 上传文件，无需先将整个文件
读入内存。例如，使用 `os.Open` 打开本地产物后（需要导入 `os`）：

```go
file, err := os.Open("artifact.tar.gz")
if err != nil {
    panic(err)
}
defer file.Close()

if err := sandbox.Files().WriteFrom(ctx, "/tmp/artifact.tar.gz", file); err != nil {
    panic(err)
}
```

此方法同样支持 `ForUser`。SDK 只发送一次 multipart 请求，不会重试或跟随
重定向，因为输入 reader 可能无法重放。reader 仍由调用方负责关闭。
服务端及代理需要支持未知长度的请求体（HTTP/1.1 使用 chunked 传输）。

使用标准 HTTP transport（包括保留其 HTTP trace hooks 的包装层）时，此方法还会
等待请求写入结果。自定义 transport 不必实现 tracing，但如果报告了 `GotConn` 和
`WroteHeaders`，就必须用 `WroteRequest` 完成该观察周期；没有这些 hooks 时，
它仍需负责自身的写入错误契约。请设置 context deadline 或 HTTP 客户端 `Timeout`，
否则缺失的完成回调可能导致无限等待。完整契约见 Go SDK README。

整个操作（包括收到响应后等待请求体完成）受 context deadline 与注入 HTTP
客户端 `Timeout` 中较早者限制。如果源 reader 的 `Read` 自身阻塞，则需要由
调用方解除阻塞，或让 reader 自身支持取消；出错后再次使用源之前，需要确认
该 `Read` 已退出。上传不是原子操作，出错时目标文件可能已被截断或部分写入。
原有 `Write([]byte)` 的行为保持不变。

此方法避免在 SDK 中缓存整个文件，自定义传输或代理仍可能缓存请求。
仓库自带的 CubeProxy 配置将整个请求体（包含 multipart 边界）的大小限制为
256 MiB；部署时可以覆盖该配置。`WriteFrom` 不会改变代理缓冲行为或上传大小限制。
