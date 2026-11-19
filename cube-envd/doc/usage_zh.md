# 在本地运行 cube-envd

[English](usage.md) · [组件概览](../README_zh.md)

组件构建独立的 Linux daemon。镜像发布、模板选择、一键安装及 SDK/平台 E2E
集成留待后续独立工作；本次改动不为平台安装或选择 daemon。

## 构建与启动

从仓库根目录执行，环境应为允许 API 修改文件和进程的可丢弃 Linux guest 或
容器。完整测试使用[组件隔离环境](../README_zh.md#测试)。

```bash
make -C cube-envd build
cube-envd/target/debug/cube-envd -version
cube-envd/target/debug/cube-envd cube-version
cube-envd/target/debug/cube-envd -commit
cube-envd/target/debug/cube-envd -isnotfc -port 49983
```

设置了 `CARGO_TARGET_DIR` 时，改用其 `debug/cube-envd` 程序。最后一条命令在
前台运行；在同一环境内打开另一个终端执行下文请求。`-isnotfc` 关闭
Firecracker MMDS/日志导出任务。可写 cgroup v2 可启用 envd 进程分组；初始化失败
时记录警告并使用既有 no-op 降级。

## 健康检查与文件操作

```bash
export NO_PROXY=localhost,127.0.0.1,::1
export no_proxy="$NO_PROXY"
curl -fsS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:49983/health
# 204

printf 'hello from cube-envd\n你好\n' | curl -fsS \
  -X POST -H 'Content-Type: application/octet-stream' --data-binary @- \
  'http://127.0.0.1:49983/files?path=/tmp/envd-example.txt&username=root'
curl -fsS 'http://127.0.0.1:49983/files?path=/tmp/envd-example.txt&username=root'
```

这些请求直接访问刚启动的 daemon，不作为 CubeProxy、模板 READY、SDK 路由或
镜像生命周期验收。若 `/init` 已配置访问令牌，应附加 `X-Access-Token` header。
初始化、Process/PTY 流、Filesystem RPC、gzip/multipart、签名文件请求及错误行为
见 [API 文档](cube-envd-api_zh.md)。

完成后按 Ctrl-C 停止前台 daemon，并在可丢弃环境删除示例文件。运行进程、文件和
协议回归测试的方法见[隔离测试说明](../README_zh.md#测试)。
