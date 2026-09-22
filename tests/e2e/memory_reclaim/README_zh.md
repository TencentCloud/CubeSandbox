# 空闲页上报 E2E

该节点本地测试验证通过 CubeAPI 创建的沙箱在保持运行时，能否将 Guest 中释放的页面归还宿主机。测试会在 Cube 暂停/恢复后重复相同的高低内存周期，以覆盖快照恢复路径。

执行器必须运行在选定的 Cubelet 节点上，因为它会从 `/proc/<pid>/status` 读取 VMM 进程 RSS。执行器会优先通过 `vmm.pid` 查找进程，在确认 PID 属于目标沙箱后使用它；如果运行时在启动后删除了 containerd task-state 目录，则回退到 CubeShim 命令行查找。使用 `--node` 固定沙箱，避免把宿主机 RSS 样本关联到运行在其他节点上的沙箱。

```bash
CUBE_API_URL=http://127.0.0.1:3000 \
CUBE_E2E_IMAGE=registry.example.com/sandbox-code:tag \
CUBE_E2E_NODE=node-name \
tests/e2e/memory_reclaim/free_page_reporting.sh
```

使用 `--image` 会基于当前部署的组件构建一个临时 2 GiB 模板，并在测试后删除。创建响应返回后，执行器会立即跟踪该模板，包括构建失败和超时路径。这是推荐的默认连线检查方式：旧模板会保留旧快照保存的设备拓扑，因此恢复时不会新增 balloon 设备。

空闲页上报在 aarch64 上默认关闭，因此在该架构运行此回收测试时，Cubelet 环境中必须包含 `CUBE_BALLOON_FREE_PAGE_REPORTING=on`。请先设置该变量并重启 Cubelet，使其内置 containerd 继承新值，然后再创建临时模板或 redo 现有模板。Helm 部署应将该变量加入 `cubeNode.env`；one-click 部署应将其加入发布包 `.env` 并重新运行安装器。已经运行的 shim 和现有快照仍保留原设备拓扑。

如需验证旧模板的升级路径，请先使用升级后的组件 redo 该模板，再通过 `--template` 传入其 ID。Redo 是显式的测试前置步骤，E2E 执行器不会自动执行。

默认负载会分配并触碰 1,536 MiB 内存。测试会等待 Guest 工作负载的 ready 标记后再采集峰值，确保释放前已触碰完整映射。测试要求 RSS 至少增长 1,024 MiB；释放后则等待 RSS 回落到执行负载前基线以上 384 MiB 以内，但不强制固定的回收延迟。冷启动和暂停/恢复两个阶段都会记录实际回收量。

当 Cubelet 的 containerd task-state 目录不在标准的 `/data/cubelet/state` 或 `/data/cubelet/root` 布局下时，请使用 `--runtime-state-root`。脚本默认销毁沙箱，并等待 API 资源、shim 进程和 task-state 目录全部消失。清理失败会使 E2E 失败；`--keep-resources` 仅用于调试。
