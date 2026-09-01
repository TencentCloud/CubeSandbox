# Protocol Documentation
<a name="top"></a>

## Table of Contents

- [filesystem/filesystem.proto](#filesystem_filesystem-proto)
    - [CreateWatcherRequest](#filesystem-CreateWatcherRequest)
    - [CreateWatcherResponse](#filesystem-CreateWatcherResponse)
    - [EntryInfo](#filesystem-EntryInfo)
    - [EntryInfo.MetadataEntry](#filesystem-EntryInfo-MetadataEntry)
    - [FilesystemEvent](#filesystem-FilesystemEvent)
    - [GetWatcherEventsRequest](#filesystem-GetWatcherEventsRequest)
    - [GetWatcherEventsResponse](#filesystem-GetWatcherEventsResponse)
    - [ListDirRequest](#filesystem-ListDirRequest)
    - [ListDirResponse](#filesystem-ListDirResponse)
    - [MakeDirRequest](#filesystem-MakeDirRequest)
    - [MakeDirResponse](#filesystem-MakeDirResponse)
    - [MoveRequest](#filesystem-MoveRequest)
    - [MoveResponse](#filesystem-MoveResponse)
    - [RemoveRequest](#filesystem-RemoveRequest)
    - [RemoveResponse](#filesystem-RemoveResponse)
    - [RemoveWatcherRequest](#filesystem-RemoveWatcherRequest)
    - [RemoveWatcherResponse](#filesystem-RemoveWatcherResponse)
    - [StatRequest](#filesystem-StatRequest)
    - [StatResponse](#filesystem-StatResponse)
    - [WatchDirRequest](#filesystem-WatchDirRequest)
    - [WatchDirResponse](#filesystem-WatchDirResponse)
    - [WatchDirResponse.KeepAlive](#filesystem-WatchDirResponse-KeepAlive)
    - [WatchDirResponse.StartEvent](#filesystem-WatchDirResponse-StartEvent)
  
    - [EventType](#filesystem-EventType)
    - [FileType](#filesystem-FileType)
  
    - [Filesystem](#filesystem-Filesystem)
  
- [process/process.proto](#process_process-proto)
    - [CloseStdinRequest](#process-CloseStdinRequest)
    - [CloseStdinResponse](#process-CloseStdinResponse)
    - [ConnectRequest](#process-ConnectRequest)
    - [ConnectResponse](#process-ConnectResponse)
    - [ListRequest](#process-ListRequest)
    - [ListResponse](#process-ListResponse)
    - [PTY](#process-PTY)
    - [PTY.Size](#process-PTY-Size)
    - [ProcessConfig](#process-ProcessConfig)
    - [ProcessConfig.EnvsEntry](#process-ProcessConfig-EnvsEntry)
    - [ProcessEvent](#process-ProcessEvent)
    - [ProcessEvent.DataEvent](#process-ProcessEvent-DataEvent)
    - [ProcessEvent.EndEvent](#process-ProcessEvent-EndEvent)
    - [ProcessEvent.KeepAlive](#process-ProcessEvent-KeepAlive)
    - [ProcessEvent.StartEvent](#process-ProcessEvent-StartEvent)
    - [ProcessInfo](#process-ProcessInfo)
    - [ProcessInput](#process-ProcessInput)
    - [ProcessSelector](#process-ProcessSelector)
    - [SendInputRequest](#process-SendInputRequest)
    - [SendInputResponse](#process-SendInputResponse)
    - [SendSignalRequest](#process-SendSignalRequest)
    - [SendSignalResponse](#process-SendSignalResponse)
    - [StartRequest](#process-StartRequest)
    - [StartResponse](#process-StartResponse)
    - [StreamInputRequest](#process-StreamInputRequest)
    - [StreamInputRequest.DataEvent](#process-StreamInputRequest-DataEvent)
    - [StreamInputRequest.KeepAlive](#process-StreamInputRequest-KeepAlive)
    - [StreamInputRequest.StartEvent](#process-StreamInputRequest-StartEvent)
    - [StreamInputResponse](#process-StreamInputResponse)
    - [UpdateRequest](#process-UpdateRequest)
    - [UpdateResponse](#process-UpdateResponse)
  
    - [Signal](#process-Signal)
  
    - [Process](#process-Process)
  
- [Scalar Value Types](#scalar-value-types)



<a name="filesystem_filesystem-proto"></a>
<p align="right"><a href="#top">Top</a></p>

## filesystem/filesystem.proto



<a name="filesystem-CreateWatcherRequest"></a>

### CreateWatcherRequest
表示创建持久 watcher 的请求，当前服务不实现。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| path | [string](#string) |  | 要监听的目录路径。 |
| recursive | [bool](#bool) |  | 是否递归监听子目录。 |
| include_entry | [bool](#bool) |  | 为 true 时，事件会在可用情况下附带受影响条目的元数据。 |
| allow_network_mounts | [bool](#bool) |  | 为 true 时，允许监听网络文件系统挂载。 网络挂载上的事件可能不可靠或完全不会送达。 |






<a name="filesystem-CreateWatcherResponse"></a>

### CreateWatcherResponse
返回持久 watcher 的标识符，当前服务不实现。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| watcher_id | [string](#string) |  | 持久 watcher 的唯一标识符。 |






<a name="filesystem-EntryInfo"></a>

### EntryInfo
描述文件、目录或符号链接的元数据。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| name | [string](#string) |  | 条目的基名。 |
| type | [FileType](#filesystem-FileType) |  | 条目的文件类型。 |
| path | [string](#string) |  | 条目的完整路径。 |
| size | [int64](#int64) |  | 条目大小，单位为字节。 |
| mode | [uint32](#uint32) |  | Unix 权限位。 |
| permissions | [string](#string) |  | 人类可读的 rwx 权限字符串。 |
| owner | [string](#string) |  | 条目属主名称。 |
| group | [string](#string) |  | 条目属组名称。 |
| modified_time | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  | 条目的最后修改时间。 |
| symlink_target | [string](#string) | optional | 条目为符号链接时，保存其目标路径。 |
| metadata | [EntryInfo.MetadataEntry](#filesystem-EntryInfo-MetadataEntry) | repeated | 由用户定义并存储在文件扩展属性中的元数据。 键位于 user.e2b. 命名空间，返回时会移除该前缀。 其他工具写入的普通 user.* 扩展属性不会出现在这里。 |






<a name="filesystem-EntryInfo-MetadataEntry"></a>

### EntryInfo.MetadataEntry



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| key | [string](#string) |  |  |
| value | [string](#string) |  |  |






<a name="filesystem-FilesystemEvent"></a>

### FilesystemEvent
表示 WatchDir 观察到的一个文件系统事件。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| name | [string](#string) |  | 相对于监听根目录的受影响路径。 |
| type | [EventType](#filesystem-EventType) |  | 事件类型。 |
| entry | [EntryInfo](#filesystem-EntryInfo) | optional | 触发事件的条目元数据；只有请求 include_entry 且条目仍可 stat 时才会填充。 删除或移走后的重命名事件通常不包含此字段，因为原路径上的条目已不存在。 |






<a name="filesystem-GetWatcherEventsRequest"></a>

### GetWatcherEventsRequest
表示轮询持久 watcher 事件的请求，当前服务不实现。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| watcher_id | [string](#string) |  | 要轮询的 watcher 标识符。 |






<a name="filesystem-GetWatcherEventsResponse"></a>

### GetWatcherEventsResponse
返回持久 watcher 累积事件，当前服务不实现。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| events | [FilesystemEvent](#filesystem-FilesystemEvent) | repeated | 已收集的文件系统事件。 |






<a name="filesystem-ListDirRequest"></a>

### ListDirRequest
表示列出目录的请求。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| path | [string](#string) |  | 要枚举的目录路径。 |
| depth | [uint32](#uint32) |  | 要递归列出的最大深度。 |






<a name="filesystem-ListDirResponse"></a>

### ListDirResponse
返回按请求深度收集的目录条目。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| entries | [EntryInfo](#filesystem-EntryInfo) | repeated | 已收集的条目。 |






<a name="filesystem-MakeDirRequest"></a>

### MakeDirRequest
表示创建目录的请求。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| path | [string](#string) |  | 要创建的目录路径。 |






<a name="filesystem-MakeDirResponse"></a>

### MakeDirResponse
返回新建目录的元数据。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| entry | [EntryInfo](#filesystem-EntryInfo) |  | 已创建目录的元数据。 |






<a name="filesystem-MoveRequest"></a>

### MoveRequest
表示移动条目的源路径和目标路径。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| source | [string](#string) |  | 要移动的现有路径。 |
| destination | [string](#string) |  | 移动后的目标路径。 |






<a name="filesystem-MoveResponse"></a>

### MoveResponse
返回移动后的目标条目元数据。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| entry | [EntryInfo](#filesystem-EntryInfo) |  | 已移动条目的元数据。 |






<a name="filesystem-RemoveRequest"></a>

### RemoveRequest
表示删除文件或目录的请求。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| path | [string](#string) |  | 要删除的路径。 |






<a name="filesystem-RemoveResponse"></a>

### RemoveResponse
表示没有响应字段的删除结果。






<a name="filesystem-RemoveWatcherRequest"></a>

### RemoveWatcherRequest
表示移除持久 watcher 的请求，当前服务不实现。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| watcher_id | [string](#string) |  | 要移除的 watcher 标识符。 |






<a name="filesystem-RemoveWatcherResponse"></a>

### RemoveWatcherResponse
表示没有响应字段的移除 watcher 结果。






<a name="filesystem-StatRequest"></a>

### StatRequest
表示查询条目元数据的请求。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| path | [string](#string) |  | 要查询的路径。 |






<a name="filesystem-StatResponse"></a>

### StatResponse
返回指定路径的条目元数据。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| entry | [EntryInfo](#filesystem-EntryInfo) |  | 查询到的条目元数据。 |






<a name="filesystem-WatchDirRequest"></a>

### WatchDirRequest
表示建立流式目录监听的请求。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| path | [string](#string) |  | 要监听的目录路径。 |
| recursive | [bool](#bool) |  | 是否递归监听子目录。 |
| include_entry | [bool](#bool) |  | 为 true 时，事件会在可用情况下附带受影响条目的元数据。 |
| allow_network_mounts | [bool](#bool) |  | 为 true 时，允许监听 NFS、CIFS、SMB、FUSE 等网络文件系统挂载。 网络挂载上的事件可能不可靠或完全不会送达。 |






<a name="filesystem-WatchDirResponse"></a>

### WatchDirResponse
表示 WatchDir 服务端流中的一个事件。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| start | [WatchDirResponse.StartEvent](#filesystem-WatchDirResponse-StartEvent) |  | 表示 watcher 已成功建立。 |
| filesystem | [FilesystemEvent](#filesystem-FilesystemEvent) |  | 表示一个文件系统变化事件。 |
| keepalive | [WatchDirResponse.KeepAlive](#filesystem-WatchDirResponse-KeepAlive) |  | 表示空闲流保活事件。 |






<a name="filesystem-WatchDirResponse-KeepAlive"></a>

### WatchDirResponse.KeepAlive
表示没有额外字段的保活事件。






<a name="filesystem-WatchDirResponse-StartEvent"></a>

### WatchDirResponse.StartEvent
表示没有额外字段的 watcher 启动事件。





 


<a name="filesystem-EventType"></a>

### EventType
定义目录监听可报告的文件系统事件类型。

| Name | Number | Description |
| ---- | ------ | ----------- |
| EVENT_TYPE_UNSPECIFIED | 0 | 未指定或无法映射的事件类型。 |
| EVENT_TYPE_CREATE | 1 | 创建文件或目录。 |
| EVENT_TYPE_WRITE | 2 | 写入文件内容或其他可写变更。 |
| EVENT_TYPE_REMOVE | 3 | 删除文件或目录。 |
| EVENT_TYPE_RENAME | 4 | 重命名文件或目录。 |
| EVENT_TYPE_CHMOD | 5 | 修改权限或所有权。 |



<a name="filesystem-FileType"></a>

### FileType
定义 EntryInfo 支持的文件系统条目类型。

| Name | Number | Description |
| ---- | ------ | ----------- |
| FILE_TYPE_UNSPECIFIED | 0 | 未指定或无法识别的类型。 |
| FILE_TYPE_FILE | 1 | 常规文件。 |
| FILE_TYPE_DIRECTORY | 2 | 目录。 |
| FILE_TYPE_SYMLINK | 3 | 符号链接。 |


 

 


<a name="filesystem-Filesystem"></a>

### Filesystem
提供文件、目录和流式目录监听接口。

| Method Name | Request Type | Response Type | Description |
| ----------- | ------------ | ------------- | ------------|
| Stat | [StatRequest](#filesystem-StatRequest) | [StatResponse](#filesystem-StatResponse) | 查询单个文件系统条目的元数据。 |
| MakeDir | [MakeDirRequest](#filesystem-MakeDirRequest) | [MakeDirResponse](#filesystem-MakeDirResponse) | 创建目录及其缺失父目录。 |
| Move | [MoveRequest](#filesystem-MoveRequest) | [MoveResponse](#filesystem-MoveResponse) | 移动文件或目录条目。 |
| ListDir | [ListDirRequest](#filesystem-ListDirRequest) | [ListDirResponse](#filesystem-ListDirResponse) | 按指定深度列出目录条目。 |
| Remove | [RemoveRequest](#filesystem-RemoveRequest) | [RemoveResponse](#filesystem-RemoveResponse) | 删除文件或目录条目。 |
| WatchDir | [WatchDirRequest](#filesystem-WatchDirRequest) | [WatchDirResponse](#filesystem-WatchDirResponse) stream | 流式订阅目录中的文件系统事件。 |
| CreateWatcher | [CreateWatcherRequest](#filesystem-CreateWatcherRequest) | [CreateWatcherResponse](#filesystem-CreateWatcherResponse) | 创建持久 watcher，当前服务不实现。 |
| GetWatcherEvents | [GetWatcherEventsRequest](#filesystem-GetWatcherEventsRequest) | [GetWatcherEventsResponse](#filesystem-GetWatcherEventsResponse) | 轮询持久 watcher 事件，当前服务不实现。 |
| RemoveWatcher | [RemoveWatcherRequest](#filesystem-RemoveWatcherRequest) | [RemoveWatcherResponse](#filesystem-RemoveWatcherResponse) | 移除持久 watcher，当前服务不实现。 |

 



<a name="process_process-proto"></a>
<p align="right"><a href="#top">Top</a></p>

## process/process.proto



<a name="process-CloseStdinRequest"></a>

### CloseStdinRequest
表示关闭普通 stdin 的请求。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| process | [ProcessSelector](#process-ProcessSelector) |  | 要关闭 stdin 的进程。 |






<a name="process-CloseStdinResponse"></a>

### CloseStdinResponse
表示没有响应字段的关闭 stdin 结果。






<a name="process-ConnectRequest"></a>

### ConnectRequest
表示订阅进程输出的请求。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| process | [ProcessSelector](#process-ProcessSelector) |  | 要订阅的进程。 |






<a name="process-ConnectResponse"></a>

### ConnectResponse
包含 Connect 流中一个进程事件的响应。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| event | [ProcessEvent](#process-ProcessEvent) |  | 要发送给客户端的事件。 |






<a name="process-ListRequest"></a>

### ListRequest
表示无需参数的进程列表请求。






<a name="process-ListResponse"></a>

### ListResponse
包含当前存活进程列表的响应。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| processes | [ProcessInfo](#process-ProcessInfo) | repeated | 当前由 envd 管理的进程。 |






<a name="process-PTY"></a>

### PTY
描述可选的伪终端及其尺寸。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| size | [PTY.Size](#process-PTY-Size) |  | 初始或更新后的终端尺寸。 |






<a name="process-PTY-Size"></a>

### PTY.Size
描述终端的列数和行数。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| cols | [uint32](#uint32) |  | 终端列数。 |
| rows | [uint32](#uint32) |  | 终端行数。 |






<a name="process-ProcessConfig"></a>

### ProcessConfig
描述要启动或在列表中返回的进程配置。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| cmd | [string](#string) |  | 要执行的程序路径或命令。 |
| args | [string](#string) | repeated | 传递给程序的参数列表。 |
| envs | [ProcessConfig.EnvsEntry](#process-ProcessConfig-EnvsEntry) | repeated | 覆盖默认环境变量的键值对。 |
| cwd | [string](#string) | optional | 可选的工作目录。 |






<a name="process-ProcessConfig-EnvsEntry"></a>

### ProcessConfig.EnvsEntry



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| key | [string](#string) |  |  |
| value | [string](#string) |  |  |






<a name="process-ProcessEvent"></a>

### ProcessEvent
表示进程输出流中的一个服务端事件。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| start | [ProcessEvent.StartEvent](#process-ProcessEvent-StartEvent) |  | 进程已启动事件。 |
| data | [ProcessEvent.DataEvent](#process-ProcessEvent-DataEvent) |  | 标准输出、标准错误或 PTY 输出事件。 |
| end | [ProcessEvent.EndEvent](#process-ProcessEvent-EndEvent) |  | 进程结束事件。 |
| keepalive | [ProcessEvent.KeepAlive](#process-ProcessEvent-KeepAlive) |  | 空闲流保活事件。 |






<a name="process-ProcessEvent-DataEvent"></a>

### ProcessEvent.DataEvent
携带一种进程输出通道的数据。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| stdout | [bytes](#bytes) |  | base64 编码的标准输出字节。 |
| stderr | [bytes](#bytes) |  | base64 编码的标准错误字节。 |
| pty | [bytes](#bytes) |  | base64 编码的 PTY 输出字节。 |






<a name="process-ProcessEvent-EndEvent"></a>

### ProcessEvent.EndEvent
描述进程的最终退出状态。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| exit_code | [sint32](#sint32) |  | 进程退出码，回收失败时为负值。 |
| exited | [bool](#bool) |  | 是否以正常退出状态结束。 |
| status | [string](#string) |  | 人类可读的退出状态。 |
| error | [string](#string) | optional | 可选的异常或回收错误。 |






<a name="process-ProcessEvent-KeepAlive"></a>

### ProcessEvent.KeepAlive
表示没有额外数据的保活事件。






<a name="process-ProcessEvent-StartEvent"></a>

### ProcessEvent.StartEvent
携带新启动进程的 PID。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| pid | [uint32](#uint32) |  | 进程组组长 PID。 |






<a name="process-ProcessInfo"></a>

### ProcessInfo
表示进程列表中的一个进程。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| config | [ProcessConfig](#process-ProcessConfig) |  | 进程启动配置。 |
| pid | [uint32](#uint32) |  | 进程组组长 PID。 |
| tag | [string](#string) | optional | 调用方提供的可选唯一标签。 |






<a name="process-ProcessInput"></a>

### ProcessInput
表示写入 stdin 或 PTY 的输入数据。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| stdin | [bytes](#bytes) |  | base64 编码的普通 stdin 数据。 |
| pty | [bytes](#bytes) |  | base64 编码的 PTY 输入数据。 |






<a name="process-ProcessSelector"></a>

### ProcessSelector
要求调用方通过 PID 或标签二选一定位进程。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| pid | [uint32](#uint32) |  | 进程组组长 PID。 |
| tag | [string](#string) |  | 调用方提供的唯一进程标签。 |






<a name="process-SendInputRequest"></a>

### SendInputRequest
表示一元进程输入请求。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| process | [ProcessSelector](#process-ProcessSelector) |  | 要接收输入的进程。 |
| input | [ProcessInput](#process-ProcessInput) |  | 要写入的 stdin 或 PTY 数据。 |






<a name="process-SendInputResponse"></a>

### SendInputResponse
表示没有响应字段的一元输入结果。






<a name="process-SendSignalRequest"></a>

### SendSignalRequest
表示向进程发送信号的请求。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| process | [ProcessSelector](#process-ProcessSelector) |  | 要接收信号的进程。 |
| signal | [Signal](#process-Signal) |  | 要发送的受支持信号。 |






<a name="process-SendSignalResponse"></a>

### SendSignalResponse
表示没有响应字段的发送信号结果。






<a name="process-StartRequest"></a>

### StartRequest
表示启动一个进程的流式请求。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| process | [ProcessConfig](#process-ProcessConfig) |  | 必填的进程配置。 |
| pty | [PTY](#process-PTY) | optional | 可选的 PTY 配置。 |
| tag | [string](#string) | optional | 可选的唯一进程标签。 |
| stdin | [bool](#bool) | optional | 是否保留普通 stdin；为兼容旧客户端，缺失时默认为 true。 |






<a name="process-StartResponse"></a>

### StartResponse
包含 Start 流中一个进程事件的响应。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| event | [ProcessEvent](#process-ProcessEvent) |  | 要发送给客户端的事件。 |






<a name="process-StreamInputRequest"></a>

### StreamInputRequest
表示 StreamInput 客户端流中的一个事件。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| start | [StreamInputRequest.StartEvent](#process-StreamInputRequest-StartEvent) |  | 选择后续数据帧使用的进程。 |
| data | [StreamInputRequest.DataEvent](#process-StreamInputRequest-DataEvent) |  | 向已选择进程写入输入。 |
| keepalive | [StreamInputRequest.KeepAlive](#process-StreamInputRequest-KeepAlive) |  | 保持空闲客户端流存活。 |






<a name="process-StreamInputRequest-DataEvent"></a>

### StreamInputRequest.DataEvent
表示一个输入数据帧。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| input | [ProcessInput](#process-ProcessInput) |  | 要写入的输入内容。 |






<a name="process-StreamInputRequest-KeepAlive"></a>

### StreamInputRequest.KeepAlive
表示没有额外数据的客户端保活帧。






<a name="process-StreamInputRequest-StartEvent"></a>

### StreamInputRequest.StartEvent
表示流开始时选择的进程。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| process | [ProcessSelector](#process-ProcessSelector) |  | 要在后续帧中使用的进程选择器。 |






<a name="process-StreamInputResponse"></a>

### StreamInputResponse
表示没有响应字段的流输入结果。






<a name="process-UpdateRequest"></a>

### UpdateRequest
表示 PTY 进程的更新请求。


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| process | [ProcessSelector](#process-ProcessSelector) |  | 要更新的进程选择器。 |
| pty | [PTY](#process-PTY) | optional | 包含新尺寸的 PTY 配置。 |






<a name="process-UpdateResponse"></a>

### UpdateResponse
表示没有响应字段的 PTY 更新结果。





 


<a name="process-Signal"></a>

### Signal
定义 envd 支持的进程控制信号。

| Name | Number | Description |
| ---- | ------ | ----------- |
| SIGNAL_UNSPECIFIED | 0 | 未指定信号。 |
| SIGNAL_SIGTERM | 15 | 请求进程优雅终止。 |
| SIGNAL_SIGKILL | 9 | 强制终止进程。 |


 

 


<a name="process-Process"></a>

### Process
提供进程生命周期、输出订阅和输入控制接口。

| Method Name | Request Type | Response Type | Description |
| ----------- | ------------ | ------------- | ------------|
| List | [ListRequest](#process-ListRequest) | [ListResponse](#process-ListResponse) | 列出当前由 envd 管理的存活进程。 |
| Connect | [ConnectRequest](#process-ConnectRequest) | [ConnectResponse](#process-ConnectResponse) stream | 订阅指定进程的输出和结束事件。 |
| Start | [StartRequest](#process-StartRequest) | [StartResponse](#process-StartResponse) stream | 启动进程并流式返回输出和结束事件。 |
| Update | [UpdateRequest](#process-UpdateRequest) | [UpdateResponse](#process-UpdateResponse) | 更新 PTY 进程的终端尺寸。 |
| StreamInput | [StreamInputRequest](#process-StreamInputRequest) stream | [StreamInputResponse](#process-StreamInputResponse) | 客户端输入流按帧顺序写入同一进程。 |
| SendInput | [SendInputRequest](#process-SendInputRequest) | [SendInputResponse](#process-SendInputResponse) | 向进程的普通 stdin 或 PTY 写入一段输入。 |
| SendSignal | [SendSignalRequest](#process-SendSignalRequest) | [SendSignalResponse](#process-SendSignalResponse) | 向进程组发送受支持的终止信号。 |
| CloseStdin | [CloseStdinRequest](#process-CloseStdinRequest) | [CloseStdinResponse](#process-CloseStdinResponse) | 关闭普通进程 stdin 以发送 EOF；PTY 进程应发送 Ctrl&#43;D（0x04）。 |

 



## Scalar Value Types

| .proto Type | Notes | C++ | Java | Python | Go | C# | PHP | Ruby |
| ----------- | ----- | --- | ---- | ------ | -- | -- | --- | ---- |
| <a name="double" /> double |  | double | double | float | float64 | double | float | Float |
| <a name="float" /> float |  | float | float | float | float32 | float | float | Float |
| <a name="int32" /> int32 | Uses variable-length encoding. Inefficient for encoding negative numbers – if your field is likely to have negative values, use sint32 instead. | int32 | int | int | int32 | int | integer | Bignum or Fixnum (as required) |
| <a name="int64" /> int64 | Uses variable-length encoding. Inefficient for encoding negative numbers – if your field is likely to have negative values, use sint64 instead. | int64 | long | int/long | int64 | long | integer/string | Bignum |
| <a name="uint32" /> uint32 | Uses variable-length encoding. | uint32 | int | int/long | uint32 | uint | integer | Bignum or Fixnum (as required) |
| <a name="uint64" /> uint64 | Uses variable-length encoding. | uint64 | long | int/long | uint64 | ulong | integer/string | Bignum or Fixnum (as required) |
| <a name="sint32" /> sint32 | Uses variable-length encoding. Signed int value. These more efficiently encode negative numbers than regular int32s. | int32 | int | int | int32 | int | integer | Bignum or Fixnum (as required) |
| <a name="sint64" /> sint64 | Uses variable-length encoding. Signed int value. These more efficiently encode negative numbers than regular int64s. | int64 | long | int/long | int64 | long | integer/string | Bignum |
| <a name="fixed32" /> fixed32 | Always four bytes. More efficient than uint32 if values are often greater than 2^28. | uint32 | int | int | uint32 | uint | integer | Bignum or Fixnum (as required) |
| <a name="fixed64" /> fixed64 | Always eight bytes. More efficient than uint64 if values are often greater than 2^56. | uint64 | long | int/long | uint64 | ulong | integer/string | Bignum |
| <a name="sfixed32" /> sfixed32 | Always four bytes. | int32 | int | int | int32 | int | integer | Bignum or Fixnum (as required) |
| <a name="sfixed64" /> sfixed64 | Always eight bytes. | int64 | long | int/long | int64 | long | integer/string | Bignum |
| <a name="bool" /> bool |  | bool | boolean | boolean | bool | bool | boolean | TrueClass/FalseClass |
| <a name="string" /> string | A string must always contain UTF-8 encoded or 7-bit ASCII text. | string | String | str/unicode | string | string | string | String (UTF-8) |
| <a name="bytes" /> bytes | May contain any arbitrary sequence of bytes. | string | ByteString | str | []byte | ByteString | string | String (ASCII-8BIT) |

