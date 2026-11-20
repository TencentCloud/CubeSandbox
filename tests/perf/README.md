# 性能测试

`tests/perf` 包含通用性能测试和 JuiceFS 生命周期测试两组测试，并提供并发搜索和报告工具。

- `cubebench.sh`：通用性能测试，覆盖冷启动、部署密度、快照、回滚、克隆、暂停和恢复，生成 Markdown 报告。
- `cubebench.sh search`：根据 3.2、4.3 的创建 P95 或 4.6 的恢复 P95，搜索满足 P95 ≤ 100 ms 的并发上限。
- `cubebench_jfs_local.py`：在控制节点本机执行 JuiceFS 本地生命周期测试，输出 JSON。
- `cubebench_jfs_cross_node.py`：在控制节点本机执行 JuiceFS 跨节点测试，输出 JSON。
- `cubebench_jfs_report.py`：将上述两个 JSON 渲染为第 4.1–4.4 节对应的 Markdown 表格。

请在仓库根目录执行本文命令。性能测试应在专用测试集群运行，不要与生产负载混用。

## 通用性能测试 `cubebench.sh`

### 前置条件

运行 3.x 测试前先构建测试镜像：

```bash
make -C examples/cube-bench
```

4.x 测试需要 `python3-venv`，脚本会在临时虚拟环境中安装本仓库的 Python SDK。运行机还需要能访问 CubeAPI、CubeProxy 和 CubeMaster，并安装 `cubemastercli`。

测试会创建大量沙箱。应按 `cubebench.sh` 文件开头的说明提高各 Cubelet 节点的 `host.quota`，否则结果可能受调度配额限制。

> **警告：** `run` 会在每项测试前删除集群所有节点上的全部现有沙箱，但不会删除模板。脚本默认要求交互确认；仅应在确认可清空的测试集群使用 `-y` 或 `CUBE_BENCH_YES=1`。

### 常用命令

```bash
# 查看帮助和可选测试章节
bash tests/perf/cubebench.sh help
bash tests/perf/cubebench.sh sections

# 执行全部测试（执行前交互确认）
bash tests/perf/cubebench.sh

# 仅测试 4.3“从快照创建沙箱”
bash tests/perf/cubebench.sh run 4.3

# 执行整个第 4 章；-y 跳过删除沙箱的确认
bash tests/perf/cubebench.sh run -y 4

# 将报告写到指定文件
OUTPUT_MD=/tmp/cubebench-4.3.md \
  bash tests/perf/cubebench.sh run 4.3
```

多节点集群示例：

```bash
CUBE_MASTER_IP=21.130.247.183 \
CUBE_API_URL=http://21.130.247.183:3000 \
CUBE_PROXY_NODE_IP=21.130.247.183 \
  bash tests/perf/cubebench.sh run 3.2 4
```

3.3 会读取运行机的内存数据，因此只适用于运行机本身就是 Cubelet 的单节点部署；多节点环境会跳过该项。完整环境变量和故障排查说明可执行 `help` 或查看 `cubebench.sh` 文件头部。

### 按 P95 延迟目标搜索并发

从仓库根目录运行 `cubebench.sh search`，搜索满足 P95 ≤ 100 ms 的并发上限。默认参考最近一次完整搜索中同一章节的记录。首次运行或最近一次没有该章节时，从并发 16 开始。

| 章节 | 判定指标 | 并发方式 |
|---|---|---|
| 3.2 从模板创建 | 创建 P95 | 滑动窗口。一个请求完成后即可发起下一个请求。 |
| 4.3 从快照创建 | 创建 P95 | 同步波次。等整波完成后再发下一波。 |
| 4.6 暂停后恢复 | 恢复 P95 | 同步波次。同一波先完成暂停，再执行恢复。两种操作都必须成功。 |

每轮用 500 个样本计算 P95。4.3 和 4.6 的最后一波也按指定并发数执行。补齐的操作计入错误和吞吐统计，延迟统计只取前 500 个样本。比较优化前后的结果时，请对照同一章节。

同一章节的两轮之间默认等待 5 秒。可用 `--settle` 指定秒数，也可通过 `CUBE_BENCH_SLO_SETTLE` 设置默认值。集群实际补池所需的时间取决于负载。

搜索 4.3 或 4.6 时，默认每轮都会创建 Python 虚拟环境并安装 SDK。可在运行压测的机器上一次性准备环境，供各轮复用。

```bash
export CUBE_BENCH_SDK_VENV="$HOME/.venvs/cubebench"
python3 -m venv "$CUBE_BENCH_SDK_VENV"
"$CUBE_BENCH_SDK_VENV/bin/python" -m pip install -r examples/snapshot-rollback-clone/requirements.txt
```

如果已有装好这些依赖的虚拟环境，只需将 `CUBE_BENCH_SDK_VENV` 指向该环境的目录。这样离线运行时各轮也能直接加载 SDK。

#### 常用命令

```bash
# 查看搜索帮助、可搜索章节；预览不会访问集群
bash tests/perf/cubebench.sh search help
bash tests/perf/cubebench.sh search sections
bash tests/perf/cubebench.sh search --preview

# 正式测试前填写本集群已有的 READY 模板和快照 ID；仅 4.3 需要快照
export CUBE_TEMPLATE_ID=tpl-example
export CUBE_SNAPSHOT_ID=snap-example

# 搜索全部三个场景，执行前交互确认清理集群沙箱
bash tests/perf/cubebench.sh search

# 仅搜索 3.2，并查看最近完成的默认报告（Linux）
bash tests/perf/cubebench.sh search 3.2
cat /tmp/cubebench-search-latest/search.md
cat /tmp/cubebench-search-latest/3.2/curve.csv

# 取消同一章节两轮之间的等待
bash tests/perf/cubebench.sh search --settle 0 3.2

# 搜索第 4 章中支持的 4.3 和 4.6；-y 跳过清理确认
bash tests/perf/cubebench.sh search -y 4

# 忽略历史，全部场景从并发 16 开始
bash tests/perf/cubebench.sh search --no-history

# 指定 Markdown 报告位置
OUTPUT_MD=/tmp/cubebench-3.2.md bash tests/perf/cubebench.sh search 3.2
```

默认报告写入系统临时目录中的 `cubebench-search-YYYYMMDD_HHMMSS/search.md`。可以用 `TMPDIR` 指定临时目录。每章的 `curve.csv` 按执行顺序记录 `round,concurrency,p95_ms`，包含同一并发的各次确认测量。`OUTPUT_MD` 只指定 Markdown 报告的位置。

所有选定章节执行完毕且没有运行错误时，脚本更新同一目录中的 `cubebench-search-latest` 链接。如果某一章的历史、配置或测量出错，报告会标记 `FAILED`，其余章节继续执行。最终命令返回非零，保留原链接。可用 `--history /path/to/previous-search 3.2` 复用指定目录中已完成的 3.2 记录。

多节点集群也可以从控制节点执行，沿用上面配置的模板和快照：

```bash
CUBE_MASTER_IP=192.0.2.10 \
CUBE_API_URL=http://192.0.2.10:3000 \
CUBE_PROXY_NODE_IP=192.0.2.11 \
  bash tests/perf/cubebench.sh search 3.2 4
```

如需接入外部测量系统，可单独调用规划器。例如 `echo '[]' | python3 tests/perf/cubebench_slo_search.py` 只给出首个测点；已有本轮记录时将数组从标准输入传入。

规划器按并发取历史 P95 的中位数，通过单调回归平滑曲线，估算与 100 ms 的交点。历史只用于选点。规划器根据本轮读数修正预测，在已测通过与失败点之间做线性插值；区间收窄过慢时改用二分。

当 `next_case` 为 `null` 时，查看 `results[0].status`。在达标并发构成连续区间的假设下，报告 `confirmed_boundary` 需要对相邻的两个并发各测至少三轮。较低并发的各轮 P95 中位数须 ≤ 100 ms，较高并发的中位数须 > 100 ms。并发 1 确认超限时，报告最低并发仍超限。并发 500 确认达标时，只能得出上限至少为 500。若已测结果不满足单调性，查看 `unstable_or_non_monotonic`。每章最多 48 轮，预算耗尽仍未确认时，报告 `case_budget_exhausted`。

## JuiceFS 生命周期性能测试

### 安装 Python SDK

本机和跨节点测试都应直接在 `CUBE_CONTROL_NODE`（以下例子为 c1）运行。脚本在本机访问 Docker 中的 Cube 数据库，并运行 `cubeopscli`；不需要 SSH 到计算节点。

```bash
python3 -m venv /tmp/cube-perf-venv
source /tmp/cube-perf-venv/bin/activate
pip install -e ./sdk/python
```

### 环境变量

```bash
export CUBE_API_URL=http://<cubeapi-host>:3000
export CUBE_PROXY_NODE_IP=<cubeproxy-ip>
export CUBE_CONTROL_NODE=c1
export CUBE_NODES="c1=<c1-ip>,c2=<c2-ip>,c3=<c3-ip>"
```

`CUBE_API_URL` 可以是 CubeAPI 所在节点或负载均衡地址；`CUBE_PROXY_NODE_IP` 填 CubeProxy 所在节点的可访问 IP，两者可以不同。`CUBE_CONTROL_NODE` 是运行 Docker/MySQL 和 `cubeopscli` 的控制节点；两个脚本都在 c1 执行。

可选地复制并加载配置模板，避免在每个 shell 中重复设置变量：

```bash
cp tests/perf/env.example tests/perf/.env
${EDITOR:-vi} tests/perf/.env
source tests/perf/.env
```

### 测试场景

| `--section` | 测试内容 |
| --- | --- |
| `template` | 从模板在 `origin` 节点启动沙箱 |
| `snapshot` | 在 `origin` 节点创建快照，并等待远端数据就绪 |
| `fromsnap-local` | 从快照在 `origin` 节点启动 |
| `fromsnap-cold` | 每个样本创建新快照，再在 `target` 节点启动；不执行 warmup |
| `fromsnap-warm` | 复用一个快照在 `target` 节点启动 |
| `pause-local` | 在 `origin` 节点暂停并原地恢复 |
| `pause-cross` | 暂停后仅保留 `target` 可调度，验证跨节点恢复 |
| `local` | 执行 `template`、`snapshot`、`fromsnap-local`、`pause-local` |
| `all` | 跨节点脚本执行 `fromsnap-cold`、`fromsnap-warm`；传入 `--allow-node-isolation` 才包含 `pause-cross` |

本机脚本仅接受 `local` 及四个本机场景；它在 `origin`（默认 c2）创建、暂停并恢复沙箱。脚本本身仍在 c1 执行，以访问控制面的 Docker/MySQL。跨节点脚本仅接受跨节点场景，且 `origin` 与 `target` 必须不同。

### 完整命令

先在 c1 执行 JuiceFS 本机相关性能测试：

```bash
python3 tests/perf/cubebench_jfs_local.py \
  <tpl-id> \
  --section local \
  --origin c2 \
  --samples 10 \
  --concurrency 1 5 10 \
  --warmups 3 \
  --round-gap 10 \
  --label "$(date +%F), c2 local, bundled MinIO" \
  --out /tmp/jfs-local-perf.json
```

再在 c1 执行 c2 → c3 的跨节点测试：

```bash
python3 tests/perf/cubebench_jfs_cross_node.py \
  <tpl-id> \
  --section all \
  --origin c2 \
  --target c3 \
  --samples 10 \
  --concurrency 1 5 10 \
  --warmups 3 \
  --round-gap 10 \
  --allow-node-isolation \
  --label "$(date +%F), c2-to-c3, bundled MinIO" \
  --out /tmp/jfs-cross-node-perf.json
```

将本机和跨节点 JSON 渲染为报告第 4.1–4.4 节对应的 Markdown 表格：

```bash
python3 tests/perf/cubebench_jfs_report.py \
  /tmp/jfs-local-perf.json \
  /tmp/jfs-cross-node-perf.json \
  --out /tmp/jfs-perf-tables.md
```

> **注意：** `pause-cross` 会通过 `cubeopscli` 临时隔离除 `target` 外的节点，并在测试结束或异常时尝试恢复。只应在允许改变调度状态的专用测试集群传入 `--allow-node-isolation`。运行前后建议使用 `cubeopscli node list` 检查隔离状态。

`--samples` 必须为正数，并能被 `--concurrency` 中的每个并发数整除。例如，并发为 `1 5 10` 时，样本数可使用 `10`、`20` 等值。

与 [CubeSandbox JuiceFS 多节点性能基准测试报告](../../docs/zh/blog/posts/2026-11-04-cubesandbox-juicefs-perf-benchmark.md) 同口径对比时：整套测试开始前删除全部沙箱，并清理各节点 JuiceFS 磁盘缓存与 Linux 页缓存；测试格与批次之间不再清缓存。runtime snapshot 恢复的前一到两个批次可能仍在将 guest 页面载入本地缓存并出现瞬时探活延迟，因此正式对比建议 `--warmups 3`；`fromsnap-cold` 按设计始终不执行 warmup。

### 参数说明

```text
template                 READY 的 JFS 模板 ID
--section NAME           本机脚本默认 local；跨节点脚本默认 all
--origin NODE            被测源节点；本机脚本默认 c2
--target NODE            跨节点脚本的目标节点名，必填
--samples N              每个场景、每个并发度的有效样本总数，默认 10
--concurrency N [...]    并发度列表，默认 1 5 10
--warmups N              正式计数前的预热批次数，默认 1；runtime snapshot 场景建议 3
--round-gap SECONDS      每批测试后的固定等待时间
--gap-range MIN MAX      每批测试后的随机等待时间
--allow-node-isolation   允许 pause-cross 临时修改集群调度状态
--label TEXT             写入结果的部署、版本或对象存储说明
--out PATH               JSON 输出路径，必填；执行中持续保存部分结果
```

查看脚本自身的最新参数：

```bash
python3 tests/perf/cubebench_jfs_local.py --help
python3 tests/perf/cubebench_jfs_cross_node.py --help
```

## 结果说明

`cubebench.sh` 生成 Markdown 报告。两个 JuiceFS 生命周期脚本生成 JSON，其中：

- `rows` 保存正式样本，`warmups` 保存预热数据；
- `summary.valid=true` 表示样本数完整，且测试与清理均未报错；
- `summary.metrics.*.avg` 和 `p95` 的单位均为毫秒；
- `errors` 非零或 `valid=false` 时，不应使用该单元格的平均值和 P95；
- `skipped` 记录因参数或后端限制而跳过的场景。

生命周期脚本只清理自己创建的沙箱和快照。若进程被强制终止，仍应检查是否留有 `perf-` 前缀的快照或测试沙箱。JSON 会在每批结束后原子更新，测试中断时通常仍可定位最后完成的阶段。

结果可与 `jfs-data.html` 中相同生命周期指标做趋势对比，但不是严格复现：历史冷缓存数据可能额外清理目标节点的 JFS/Page Cache，代码版本、缓存状态、对象存储和后台负载也会影响结果。

## 脚本自测

离线单元测试不会连接真实集群：

```bash
make cubebench-test
python3 -m unittest discover -s tests/perf -p 'test_jfs_bench.py'
```
