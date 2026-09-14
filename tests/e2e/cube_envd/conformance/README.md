# cube-envd 一致性对照套件

把 cube-envd 的数据面行为与 **Go envd 基线**逐条对比，产出可归档的结果。

基线 = `e2b-dev/infra@2026.16`（envd `0.5.13`），也就是基础镜像此前固定编译的那个实现。

- 对照对象是**同一镜像里的两个二进制**：`/usr/bin/envd`（cube-envd）与 `/usr/bin/envd-go`
  （Go envd），靠 `ENVD_BIN` 切换，因此 guest 环境、镜像层、内核参数完全一致，唯一变量是实现。
- 差异清单是机器可读的 [`declared_differences.toml`](./declared_differences.toml)，
  既是对照的允许清单，也是 `cube-envd/README.md` 里差异表格的唯一事实源。

## 目录

| 文件 | 作用 |
|---|---|
| `connect_client.py` | Connect-JSON 客户端（帧编解码、一元/流式请求）、响应头白名单、动态字段掩码 |
| `scenarios.py` | 场景定义：每个场景是一组请求，每条记录只保留契约相关的部分 |
| `capture.py` | 对一个端点执行场景集，写出 fixture JSON |
| `conformance.py` | 对比两个 fixture，按允许清单判定，生成 RESULTS.md / README 表格 |
| `declared_differences.toml` | 已声明的差异（含依据）+ 已决定与基线保持一致的行为 |

## 用法

```bash
cd tests/e2e/cube_envd/conformance

# 1) 采集两侧（必须用同一份 scenarios.py）
python3 capture.py --endpoint http://127.0.0.1:49985 --implementation go-envd-0.5.13 \
    --user root --out /tmp/go.json
python3 capture.py --endpoint http://127.0.0.1:49984 --implementation cube-envd-0.1.0 \
    --user root --out /tmp/cube-envd.json

# 2) 对比（--results 生成可归档的结果文件）
python3 conformance.py diff --baseline /tmp/go.json --candidate /tmp/cube-envd.json \
    --results RESULTS.md

# 3) 差异清单与 README 表格的漂移门禁
python3 conformance.py check-docs
```

`--user` 是请求要执行的本地用户（默认当前用户）。两侧必须用同一个取值：参考实现会在
"切到别的用户"时调用 `setgroups/setgid/setuid`，非 root 环境下会 EPERM——那属于环境差异，
不是协议差异。

## 判定口径

| 判定 | 含义 |
|---|---|
| `EQUAL` | 两侧逐字段一致 |
| `DECLARED-DIFF` | 差异已在 `declared_differences.toml` 声明 |
| `UNDECLARED-DIFF` | 未声明的差异 → **失败**（本套件存在的意义） |
| `MISSING-BASELINE` / `MISSING-CANDIDATE` | 一侧缺少记录 → **失败** |
| `DECLARED-BUT-EQUAL` | 清单里声明了差异、实际已一致 → 提示删除；`--strict` 下失败 |

最后一条是刻意设计的：把已经追平的差异留在允许清单里，等于给该场景永久放行，未来的回归
会被静默吞掉。

## 归一化（以及为什么只放宽这些）

归一化写在 `connect_client.py` 里，规则保持最小且显式：

- **动态取值掩码**：`pid` / `ts` / `modifiedTime` / `watcherId` 与时间戳文本 → `<time>`；
  `/tmp/cube-envd-conformance/...` 这类临时路径 → `<tmp>`。值为 `Last-Modified` 的头部只比较
  是否存在（mtime 必然不同）。
- **JSON 响应按掩码后的结构比较**：原始摘要会被上面的动态字段带偏。
- **并发数据事件的相对顺序**：一段连续的数据事件按流名排序比较。stdout/stderr 是两条独立
  管道，`/bin/sh` 在 stdout 接管道时会缓冲、stderr 不缓冲，谁先到达取决于读取调度与缓冲，
  不属于协议契约；事件集合、内容、以及它们与 start/end/keepalive 的相对位置仍然逐条比对。

不放宽的部分：状态码、白名单响应头、事件种类与内容、错误码、二进制响应体（逐字节摘要）。

## 新增场景 / 新增差异

- 新场景必须对应契约里的一句话（README 的 API 表、差异清单，或 issue #1227 的验收条件），
  不要为了"多测点"堆请求；场景之间不能共享状态。
- 新差异写进 `declared_differences.toml` 并给出基线取值、我们的取值与依据，然后
  `python3 conformance.py render-docs` 刷新 README 表格；CI 会断言两者一致。
- 只为**已观测到**的差异登记；`--strict` 会把过期的条目判失败。

## CI

`.github/workflows/cube-envd-conformance.yml` 会在构建基础镜像后，
用同一镜像起两个容器（`ENVD_BIN=/usr/bin/envd` 与 `/usr/bin/envd-go`）完成采集与对比，
并跑 `check-docs`。
