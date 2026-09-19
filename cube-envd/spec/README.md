# cube-envd 协议参考

本实现所对标的权威上游契约：

- 仓库：`e2b-dev/infra`（镜像为 `e2b-dev/runtime`），基础镜像 ref `2026.16`。
- REST OpenAPI：`packages/envd/spec/envd.yaml`。
- 进程 RPC：`packages/envd/spec/process/process.proto`。
- 文件系统 RPC：`packages/envd/spec/filesystem/filesystem.proto`。

为避免复制庞大的上游接口面，这里**不**内联（vendor）`.proto`/`.yaml` 文件；
上述路径即为基础镜像 ref 变更时需要比对的快照。本文件记录 cube-envd 实现的结构，
便于评审者无需拉取上游即可核对。

## 进程（Process）

- `List(ListRequest) -> ListResponse` — `{"processes":[{"config":{"cmd","args","envs","cwd"},"pid":N,"tag":?}]}`。
- `Start` Connect 流：`start{pid}` / `data{stdout|stderr|pty}` / `end{exitCode,exited,status,error}` / `keepalive`。
  请求中的 `stdin` 缺省为 `true`，即默认分配管道 stdin（可经 `StreamInput`/`CloseStdin` 驱动）。
- `Connect`、`SendSignal{signal}`（`SIGNAL_SIGKILL`）、`SendInput{input{stdin|pty}}`、`Update{pty{size{rows,cols}}}`。
- `StreamInput`：客户端流式事件 `start{process}` → `data{input{stdin|pty}}` / `keepalive`，
  返回单个空 `StreamInputResponse`；`ProcessInput` 必须且只能设置一个分支。
- `CloseStdin{process}`：关闭管道 stdin 以向进程投递 EOF；响应为空。
- cube-envd 扩展：`end` 还携带 `termination{reason,signal,signalName,coreDumped}`。

## 文件系统（Filesystem）

- `Stat`、`ListDir{path,depth}`、`MakeDir`、`Move{source,destination}`、`Remove`。
- `EntryInfo`：`name,type,path,size,mode,permissions,owner,group,modifiedTime,symlinkTarget?`。
  `type` 为 `FILE_TYPE_FILE | FILE_TYPE_DIRECTORY | FILE_TYPE_SYMLINK`；
  符号链接按 `lstat` 语义上报，`permissions` 前缀为 `l`。
- `WatchDir(path,recursive,includeEntry,allowNetworkMounts)` — 流式。
- `CreateWatcher -> {watcherId}`、`GetWatcherEvents -> {events:[{name,type}]}`、`RemoveWatcher`。
- `EventType`：`EVENT_TYPE_CREATE | WRITE | REMOVE | RENAME | CHMOD`。

## REST

- `GET /health` → 204。
- `POST /init {envVars,defaultUser,defaultWorkdir,accessToken}` → 204。
  `/init` 在认证白名单内且会轮换 accessToken，因此沙箱内任何进程都可以重新引导并替换 token
  （与上游 envd 的引导形态一致）；`X-Access-Token` 仅在 `/init` 提供 token 后才校验。
- `GET /envs` → `{key: value}`。
- `GET /metrics` → `{ts,cpu_count,cpu_used_pct,mem_total,mem_used,mem_cache,mem_total_mib,mem_used_mib,disk_used,disk_total}`。
- `GET/POST /files?path=...` → 原始字节；支持 Range `206`、`416`、条件 `304`。
  仅提供 identity 编码：当 `Accept-Encoding` 明确不接受 identity 时返回 `406`。
- `POST /files/compose` → `501 unimplemented`（保留在路由表中，避免误导性的 404）。
- Connect 二进制 protobuf 编解码 → `501 unimplemented`（仅实现 JSON 编解码）。
- CLI `-version` 上报所模拟的上游 envd 世代 `0.5.13`（非 crate 版本）；`-commit` 上报构建短 sha。
