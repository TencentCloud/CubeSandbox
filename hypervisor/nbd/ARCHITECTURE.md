# Hypervisor NBD 模块与 nbdkit 交互架构

当 `--disk path=` 是 `nbd://` 或 `nbd+unix://` URI 时，hypervisor 不打开本地文件，而是由新增的 `nbd` crate 作为 NBD 客户端连接 nbdkit。virtio-block 设备只依赖 `DiskFile` / `AsyncIo` 这两个 trait，看不出后端是不是 NBD。

```text
--disk path=nbd+unix:///vol0?socket=/run/nbd.sock&reconnect_timeout=10
--disk path=nbd://10.0.0.1:10809/vol0?socket_timeout=30&tcp_keepalive=on
```

## 1. 架构图

```plantuml
@startuml
left to right direction

package "Guest VM" {
  [virtio-blk 驱动] as GDRV
}

package "Hypervisor (VMM 进程)" {
  package "vmm" {
    [DeviceManager\nmake_nbd_virtio_block_device()] as DM
  }
  package "virtio-devices" {
    [virtio Block\n每个 queue 一个 epoll 线程] as BLK
  }
  package "nbd crate（新增模块）" {
    [block_backend\nNbdDisk: DiskFile\nNbdAsyncIo: AsyncIo] as BB
    [NbdClient\n探测连接 + 按 queue 建引擎] as CLI
    [NbdEngine\nio_uring 引擎 / 同步引擎] as ENG
    [Tracker\n按 max_block 切分请求\ncookie 聚合与重放] as TRK
    [NbdConn + handshake\n连接、握手、断线重连] as CONN
  }
}

package "nbdkit 进程" {
  [server\n握手 + 请求分发] as SRV
  [filters] as FLT
  [plugin\nfile / memory / 自定义] as PLG
}

database "后端存储" as STORE

GDRV --> BLK : virtqueue
DM --> BB : NbdDisk::open_boxed(uri)
DM --> BLK : Block::new(disk_image)
BLK --> BB : new_async_io() / read_vectored ...
BB --> CLI
CLI --> ENG : 每个 queue 一个 new_engine()
ENG --> TRK
ENG --> CONN
CONN --> SRV : NBD 协议\nUnix socket / TCP\n每个 queue 一条连接
SRV --> FLT
FLT --> PLG
PLG --> STORE
ENG ..> BLK : eventfd 完成通知
@enduml
```

重点：

- 接入点很小：vmm 只改了 `device_manager.rs` 里的一个 `else if` 分支，并加了一个 `make_nbd_virtio_block_device()`。适配代码都在 `nbd::block_backend` 里，`block_util` 和 virtio Block 都不用改。
- 多队列对应多连接：每个 virtio queue 有自己的 `NbdEngine` 和自己的 NBD 连接。可写盘需要 nbdkit 声明 `NBD_FLAG_CAN_MULTI_CONN`，否则多队列会被拒绝。
- 两种 I/O 引擎：优先用 io_uring 引擎（`SendMsg`/`RecvMsg`，请求流水线化，scatter/gather 直接读写 guest 内存，不做拷贝），内核不支持时退回同步引擎。两者都用 eventfd 通知完成。
- 可靠性：URI 上配置 `reconnect_timeout` 后，连接断开会按指数退避重连，并把还没完成的 chunk 重放。重连后会校验 export 的大小和只读属性没有变化。
- seccomp：virtio-block 工作线程和 vmm 线程新放行了 `socket`、`connect`、`sendmsg`、`recvmsg`、`setsockopt`、`getsockopt`、`poll` 等系统调用（地址族限 `AF_UNIX`/`AF_INET`/`AF_INET6`），因为重连会发生在 I/O 线程里。

## 2. 交互流程

### 2.1 设备创建与握手

```plantuml
@startuml
participant "DeviceManager" as DM
participant "NbdDisk\n(block_backend)" as BB
participant "NbdClient" as CLI
participant "NbdConn\n(handshake)" as CONN
participant "nbdkit server" as SRV
participant "virtio Block" as BLK

DM -> DM : is_nbd_path(disk_cfg.path)
DM -> BB : open_boxed(uri, readonly, direct, num_queues, prefer_uring)
note right of BB : direct=on 直接拒绝
BB -> CLI : NbdClient::open()
CLI -> CONN : connect_with_mode()（探测连接）
CONN -> SRV : connect（Unix / TCP）
SRV --> CONN : NBDMAGIC + IHAVEOPT + server flags
CONN -> SRV : client flags（FIXED_NEWSTYLE / NO_ZEROES）
CONN -> SRV : NBD_OPT_GO（export 名，请求 INFO_BLOCK_SIZE）
SRV --> CONN : REP_INFO(EXPORT: size, flags)\nREP_INFO(BLOCK_SIZE: min/opt/max)\nREP_ACK
note right of CONN : 服务端不支持 OPT_GO 时\n退回 NBD_OPT_EXPORT_NAME
CONN --> CLI : NbdExportInfo
CLI -> CLI : 校验只读属性、MULTI_CONN 与 num_queues\n决定使用 io_uring 还是同步引擎
BB --> DM : (Box<dyn DiskFile>, id_path)
DM -> BLK : Block::new(disk_image)

== 激活设备，每个 queue 执行一次 ==
BLK -> BB : new_async_io()
BB -> CLI : new_engine()
note right of CLI : 第 1 个 queue 复用探测连接\n其余 queue 各新建连接并重新握手\n（size/flags 必须一致）
CLI --> BLK : NbdAsyncIo(NbdEngine)
BLK -> BLK : epoll 注册 engine.notifier()（COMPLETION_EVENT）
@enduml
```

### 2.2 I/O 请求（io_uring 引擎）

```plantuml
@startuml
participant "Guest" as G
participant "virtio Block\nqueue 线程" as BLK
participant "NbdEngine\n(io_uring)" as ENG
participant "Tracker" as TRK
participant "nbdkit server" as SRV
participant "plugin" as PLG

G -> BLK : virtqueue kick
BLK -> ENG : read_vectored / write_vectored / fsync\n(offset, guest iovecs, user_data)
ENG -> TRK : add_read / add_write / add_flush
TRK --> ENG : 按 max_block（上限 32 MiB）切分成多个 chunk\n每个 chunk 一个 cookie
ENG -> SRV : SendMsg：request header + payload iovec\n（多个请求流水线发送）
SRV -> PLG : handle_request → backend_pread / pwrite / flush
PLG --> SRV : 结果
SRV --> ENG : simple reply(cookie, error) [+ 读数据]
note right of ENG : RecvMsg 先收 header，\n再按 cookie 把读数据直接收进 guest iovec；\n回包可以乱序
ENG -> TRK : chunk_done(cookie)
TRK --> ENG : 所有 chunk 完成后得到 (user_data, result)
ENG -> BLK : eventfd 通知
BLK -> ENG : complete()
BLK -> G : 写 used ring + 中断
@enduml
```

同步引擎的接口和完成语义与上面一致，只是在提交调用里逐个 chunk 阻塞地发请求、收回复，完成后同样写 eventfd。

### 2.3 断线重连与重放

```plantuml
@startuml
participant "NbdEngine" as ENG
participant "Tracker" as TRK
participant "NbdConn" as CONN
participant "nbdkit（重启后）" as SRV

ENG -> ENG : send/recv 出错、EOF 或 socket_timeout 超时
alt URI 配置了 reconnect_timeout
  ENG -> CONN : try_reconnect()
  loop 在 reconnect_timeout 时间预算内，退避 10ms 翻倍至 200ms
    CONN -> SRV : connect + 握手
  end
  CONN -> CONN : 校验 size 未变、可写盘未变成只读
  ENG -> TRK : remaining_chunks()（按 request_id 和 chunk_idx 排序）
  ENG -> SRV : 按序重放所有未完成的 chunk
  SRV --> ENG : replies
  ENG -> ENG : 正常完成，guest 无感知
else 没有配置，或超过时间预算
  ENG -> ENG : 所有挂起的请求以错误码完成（返回 -errno）
end
@enduml
```

## 3. 代码索引

| 角色 | 位置 |
| --- | --- |
| VMM 接入分支 | `hypervisor/vmm/src/device_manager.rs`（`is_nbd_path` 分支、`make_nbd_virtio_block_device`） |
| DiskFile/AsyncIo 适配 | `hypervisor/nbd/src/block_backend.rs` |
| 客户端与引擎分发 | `hypervisor/nbd/src/lib.rs`（`NbdClient`、`NbdEngine`） |
| URI 解析 | `hypervisor/nbd/src/uri.rs` |
| 握手 | `hypervisor/nbd/src/handshake.rs` |
| 连接与重连 | `hypervisor/nbd/src/conn.rs` |
| 请求切分与聚合 | `hypervisor/nbd/src/tracker.rs` |
| I/O 引擎 | `hypervisor/nbd/src/uring_engine.rs`、`hypervisor/nbd/src/sync_engine.rs` |
| nbdkit 握手 | `/data/nbdkit/server/protocol-handshake-newstyle.c`（`NBD_OPT_GO`） |
| nbdkit 请求处理 | `/data/nbdkit/server/protocol.c`（`handle_request`）、`/data/nbdkit/server/connections.c`（`handle_single_connection`） |
| 端到端测试 | `hypervisor/nbd/tests/nbdkit_integration.rs`（真实 nbdkit 下测试多队列、blocksize filter、Rust plugin 和重启重连） |
