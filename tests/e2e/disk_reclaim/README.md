# RAW 可写盘回收 POC

本目录在独立 Cube VMM 中验证 ext4 / OverlayFS → virtio-blk DISCARD → RAW hole punch。模板级的 writable disk reclaim 选项已接入 CubeShim 与模板配置（含 SDK），但本目录仍是独立的机制验证工具：它自行启动 VMM，只覆盖 Host XFS + 常规 RAW 文件，不等于平台端到端验证，也不替换 systemd 服务。

实验环境为 Host XFS、可用 KVM；需要 Python 3、C 静态链接工具链、mkfs.ext4、e2fsck、debugfs 与支持 reflink 的 cp。每次使用新建 256 MiB RAW、1 vCPU / 256 MiB Guest，顺序运行。五轮测试与快照建议预留至少 12 GiB 空闲空间。

## 构建与运行

在 Linux 仓库根目录执行。输出目录必须不存在；运行用户需要 KVM 与目录访问权限。

```sh
cargo build --manifest-path hypervisor/Cargo.toml --release --locked --bin cube-hypervisor
poc_dir=/data/cube-reclaim-poc-manual
python3 tests/e2e/disk_reclaim/build_poc.py \
  --vmm hypervisor/target/release/cube-hypervisor --out "$poc_dir"
python3 "$poc_dir/run_poc.py" --base "$poc_dir" \
  --kernel /usr/local/services/cubetoolbox/cube-kernel-scf/vmlinux \
  --rounds 5 --backends async --modes off online batch --snapshot
python3 "$poc_dir/verify_poc.py" --base "$poc_dir" \
  --restore-run "$poc_dir/00-async-online"
```

同步后端用新输出目录和 backends=sync 单独测试，不重用已有运行目录。

## 模式与测量

| 模式 | 设备与 Guest 行为 |
| --- | --- |
| off | 不宣告 DISCARD，ext4 nodiscard |
| online | 明确宣告 DISCARD，ext4 discard |
| batch | 明确宣告 DISCARD，ext4 nodiscard；底层 ext4 手动 FITRIM |

batch 是机制参照，尚未实现产品化 RPC 或周期调度。沿用当前 Shim 的 barrier=0；Guest fsync 耗时不能直接当作物理介质持久化吞吐。

每轮新建 RAW 和文件系统，相邻轮反转模式顺序。记录 64 MiB 写删、1,000 个 64 KiB 文件写删、32 MiB 再写，以及 pause / full snapshot / resume。Host 采样等待不计入 Guest 操作耗时；在线回收的完成时间须看 Host 采样，不能把 unlink 返回当作回收完成。

## 校验与产物

- Guest 使用 O_DIRECT 读取保留文件和再写文件并校验。
- 开启组分别检查大、小文件至少 80% 的新增分配量下降。
- 暂停后生成 reflink 父副本；活动盘继续删除回收，父副本完整 hash 必须不变。
- verify_poc.py 仅在私有检查副本上回放 ext4 journal，再用 debugfs 验证已知内容，不修改原盘。
- 从完整快照启动新 VMM，使用父副本克隆盘，继续校验、删除和回收。
- 保留 serial.log、vmm.log、result.json、snapshot；全部完成后生成 results.json。
- 驱动在成功或异常退出时回收 VMM 子进程，磁盘与快照保留供核查，不自动删除。

Host 分配量、Host 空闲量与 Guest 空闲量分开报告；共享块不一定立即物理释放。每轮 p99 来自 1,000 次操作，不能用五个整轮样本冒充总体高置信度 p99。

本工具不覆盖崩溃持久性、多队列高并发、其他 Host 文件系统及其他后端，也不执行异步提交故障注入。
