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

此方法同样支持 `ForUser`。SDK 只消费一次 reader，不关闭它，不重试被拒绝的
上传，也不跟随重定向。实现使用一个按需读取的 goroutine、32 KiB 缓冲区和
multipart 边界。服务端及代理需要支持未知长度请求体（HTTP/1.1 使用 chunked）。
如果旧端点不支持 multipart，且可以接受完整缓冲，可考虑先尝试 octet-stream
请求的 `Write([]byte)`；`WriteFrom` 不会重放 reader 来做兼容回退。

使用标准 HTTP transport 或保留完整 trace 的包装层时，SDK 等待最终请求写入结果。
**报告 `GotConn` 和 `WroteHeaders` 的自定义 transport 必须报告 `WroteRequest`。**
缺少完成回调会返回错误。没有 tracing 的自定义 transport 仍需负责自身的写入错误。

请用 context deadline 或注入 HTTP 客户端的 `Timeout` 限制标准 transport 下的
整个操作，两者取较早值。如果都未设置，收到成功响应后的完成阶段会使用
`Config.RequestTimeout`（默认 30 秒）限制剩余请求体消费及最终写入回调的等待。
这个兜底计时从响应到达开始，不从上传开始计时。自定义 transport 必须响应取消。

**调用返回后，阻塞的源仍可能占用一个读取 goroutine 及其缓冲区。** 取消操作可以
解除 transport 的读取等待，但无法中断任意源内部的 `Read`，例如卡住的文件系统
挂载。调用方必须解除该读取的阻塞，并在复用源前确认它已退出。上传不是原子操作，
失败时目标文件可能已被截断或部分写入。

CubeProxy 默认缓冲请求，大文件可能占用代理临时磁盘空间。256 MiB 限制包含
multipart 边界，超限会返回 HTTP 413。请求体临时目录由 `client_body_temp_path`
控制，`proxy_temp_path /data/` 用于响应。SDK 的不重试保证不约束代理：仓库配置
未启用 `non_idempotent`，因此 Nginx 通常不会重试已发送给上游的 POST。部署覆盖
可以改变这些规则。错误响应最多读取 64 KiB 加一个检测字节，超长信息会标记截断。
完整传输契约和限制请参阅 Go SDK README。
