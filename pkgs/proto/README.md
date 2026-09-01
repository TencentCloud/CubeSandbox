# pkgs/proto

共享协议模块：CubeMaster ↔ Cubelet 以及 examples 之间的 gRPC 契约的**单一事实源**。

- 6 组 proto（cubebox / errorcode / images / snapshot / volumeplugin / types）+ 生成代码（`.pb.go` 入库）
- `volumeplugin/grpctarget`：volume plugin gRPC 地址规范化工具（43 行，只依赖 `strings`）
- `doc/api.md`：protoc-gen-doc 生成的共享 API 参考

## 为什么存在

协议是组件之间的合同。合同不该由某一方「持有再复印给对方」，而应放在双方都能看到的中立之地。本模块建立前，`cubebox.proto` 在 CubeMaster 与 Cubelet 各有一份，git 历史上 12 次变更 12 次都得改两边；CubeMaster 甚至为 4 个 import 站点 replace 了整个 Cubelet 模块。详见 `docs/plans/proto-unification.md`。

## 归属判定规则

**一个 proto 放哪，看它被几个模块消费：**

- 被 ≥2 个模块消费，或定义跨模块契约（即使暂时单侧消费）→ `pkgs/proto`
- 只被一个模块消费 → 留在那个模块里
- 拿不准 → 先留在实现方，等第二个消费者出现时再上移（上移是纯机械操作）

## 使用方式

消费方（CubeMaster / Cubelet / examples）在自己的 `go.mod` 里：

```
require github.com/tencentcloud/CubeSandbox/pkgs/proto v0.0.0
replace github.com/tencentcloud/CubeSandbox/pkgs/proto => ../pkgs/proto
```

## 修改协议

1. 改 `services/<name>/v1/*.proto` 或 `types/v1/*.proto`；
2. `make proto` 再生成 `.pb.go`，连同 proto 一起提交；
3. `make doc` 刷新 `doc/api.md`。

**生成代码入库**：消费方不需要装 protoc 就能构建。

## wire 兼容性红线

`proto package` 名、消息名、字段号、service 方法全名**一概不动**——它们决定 gRPC 线上的 full method name 与 proto registry 身份。改任何一项都是破坏性变更（新旧组件无法混跑）。允许动的只有：文件位置、`go_package` 路径、import 路径。

## 生成工具链

工具链固定在 builder 镜像（`docker/Dockerfile.builder`）：protoc v28.3 + protoc-gen-go v1.36.11 + protoc-gen-go-grpc v1.6.1。well-known types vendor 在 `third_party/google/protobuf/`，因此无需系统 protobuf include。
