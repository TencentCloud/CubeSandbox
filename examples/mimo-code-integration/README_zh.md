# MiMo Code 双分叉 Rollout 参考模式

[English](README.md)

本示例组合两种相互独立的分叉机制：

- MiMo Code 通过 `--session ... --fork` 从同一个规划会话派生子会话。
- CubeSandbox 通过快照从同一个完整 MicroVM 基线派生候选沙箱。

每个 MiMo 子会话在隔离的候选 MicroVM 中实现同一任务。确定性评估器会拒绝
不安全或测试失败的补丁，选择改动最小的通过方案，并只把获胜补丁提升到源
MicroVM。如果最终验证失败，CubeSandbox 会把源沙箱回滚到基线快照。

这是一套推测式编码事务，而不只是“把 Agent 跑在沙箱里”。

其中生命周期是可复用的参考模式，随附的 `fixtures/normalize-slug` 工程只是
确定性演示任务。任务说明、测试命令、可编辑路径和候选策略都位于
`task.json`，不会硬编码在编排流程中。

按照 Issue 的用例分类，它是增强版的**用沙箱执行 Agent 生成的代码并回收
结果**：候选 Agent 修改任务允许的文件，MicroVM 执行固定验收测试，Host 回收
有长度限制的测试输出与补丁元数据，再提升一个结果。

## 架构

```text
Host 驱动
  |
  +-- 带凭证的规划 MicroVM
  |     `-- MiMo 只规划父会话
  |                  |
  |                  `-- 仅复制 $MIMOCODE_HOME（不复制密钥）
  |
  +-- 源 MicroVM
  |     +-- 写入固定验收测试
  |     +-- 导入父会话，运行时不带凭证规则
  |     +-- 创建基线快照
  |     `-- 应用获胜补丁或回滚
  |
  +-- 候选 MicroVM A <- 基线快照
  |     `-- MiMo 子会话 A <- 父会话 --fork
  |
  `-- 候选 MicroVM B <- 基线快照
        `-- MiMo 子会话 B <- 父会话 --fork
```

父会话会得到一个不会写入 `/workspace` 的随机连续性令牌。每个子会话必须从
对话上下文中回忆该令牌，从而证明工作流继承的不只是 VM 文件系统，还包括
MiMo 会话上下文。

### 与现有快照示例的关系

CubeSandbox 侧遵循
[`07_clone_concurrent.py`](../snapshot-rollback-clone/07_clone_concurrent.py)
与
[`08_fork_three_axis.py`](../snapshot-rollback-clone/08_fork_three_axis.py)
验证的生命周期约束：从同一快照创建多个沙箱、继承基线状态、隔离后续写入，
并保持源沙箱可继续使用。本示例在这些纯 VM 原语之上，为每个候选配对独立的
MiMo `--fork` 对话分支，并增加确定性选优、补丁提升与回滚。

## 参考模式验证的能力

1. MiMo 只规划父会话不会修改规划沙箱的 Git 工作区。
2. 驱动把 `$MIMOCODE_HOME` 传入不带凭证的源 VM。
3. 完整 VM 快照捕获代码仓库与导入的父会话。
4. 多个候选 MicroVM 从完全相同的基线启动。
5. 每个候选中的 `mimo run --session <parent> --fork` 都产生唯一子会话。
6. 候选写入彼此隔离，也不会影响源沙箱。
7. 只有任务 `allowed_paths` 声明的文本 diff 可以提升；测试改动、其他新文件、
   二进制/模式 diff、超大补丁和失败测试都会被拒绝。被忽略的运行产物不会
   进入补丁。
8. 通过的候选按改动行数和候选名称确定性排序。
9. 获胜补丁必须通过 `git apply --check`，并在源沙箱再次通过相同测试。
10. 最终验证失败会触发 `rollback(snapshot_id)`。
11. 成功、失败或中断时都会回收规划、候选与源沙箱以及持久快照。

## 安全边界

规划和候选 MicroVM 都采用：

- `allow_internet_access=False`；
- 仅允许 `api.xiaomimimo.com` 的精确规则；
- 由 CubeEgress 在链路上注入真实 MiMo Platform `api-key`；
- VM 内只存在 `MIMO_API_KEY=cube-egress-managed-placeholder`；
- 禁用分享、遥测、自动更新、模型清单下载、LSP 下载和外部 skill。

每轮 rollout 使用随机 CubeEgress 规则名，因此证据收集器即使在共享 Host 上
也只选择本轮审计记录。

源 VM 使用默认拒绝网络且不携带凭证规则。创建快照前只向它复制含占位符的
MiMo profile，因此真实密钥不会进入快照 VM 数据或持久化创建请求。真实密钥
只存在于短生命周期规划/候选 VM 的 Host 侧 CubeEgress 规则中，不会写入 VM
环境、MiMo profile、Git 工作区、候选补丁或快照。

profile 传输最多接受 16 MiB Base64 归档和 64 MiB 解压后数据，避免 Host 侧
无上限解压。

## 目录结构

```text
mimo-code-integration/
├── Dockerfile
├── build-template.sh
├── speculative_mimo_code.py  # 可复用的分叉/测试/提升/回滚生命周期
├── rollout_task.py            # 有边界的 task.json 与 fixture 加载器
├── fixtures/
│   └── normalize-slug/        # 演示 task.json + project/
├── run_mimo_code.py          # 最小模板与 NDJSON 冒烟测试
├── network_policy.py         # CubeEgress 安全边界预检
├── env_utils.py
├── _mimo_common.py
├── collect_e2e_evidence.sh
├── requirements.txt
├── tests/
├── README.md
└── README_zh.md
```

## 前置条件

- 已运行的 CubeSandbox 部署，CubeAPI 可通过
  `http://<cube-host>:3000` 访问。
- CubeSandbox 已支持 snapshot/rollback 与 CubeEgress 凭证注入。
- Host 安装 `cubemastercli`、Docker，并有 Cube 节点可访问的镜像仓库。
- Host 使用 Python 3.10+ 与 `cubesandbox>=0.6.0`。
- 从 <https://platform.xiaomimimo.com> 获取 MiMo Platform API key。

## 1. 构建并注册模板

```bash
export MIMO_IMAGE="<your-registry>/mimo-code-cube:0.1.7"
./examples/mimo-code-integration/build-template.sh
cubemastercli tpl watch --job-id <job_id>
```

镜像固定 `@mimo-ai/cli@0.1.7`，构建时验证 `mimo --version` 和
`mimo run --fork` 参数，并继承 CubeSandbox 的 `envd` entrypoint。

## 2. 配置 Host

```bash
cd examples/mimo-code-integration
install -m 600 .env.example .env
# 设置 E2B_API_URL、E2B_API_KEY、CUBE_TEMPLATE_ID 和 MIMO_API_KEY。
python3 -m venv .venv
. .venv/bin/activate
pip install -r requirements.txt
```

重要配置：

| 变量 | 用途 |
| --- | --- |
| `E2B_API_URL` / `E2B_API_KEY` | CubeAPI 连接 |
| `CUBE_TEMPLATE_ID` | READY 状态的 MiMo 模板 ID |
| `MIMO_API_KEY` | CubeEgress 使用的 Host 侧真实密钥 |
| `MIMO_MODEL` | 默认 `mimo/mimo-v2.5-pro` |
| `MIMOCODE_HOME` | MiMo profile 根目录，默认 `/root/.mimocode` |
| `MIMO_WORKSPACE` | 候选 Git 工作区，默认 `/workspace` |
| `MIMO_SANDBOX_TIMEOUT` | 沙箱超时，默认 1800 秒 |
| `MIMO_AGENT_EXEC_TIMEOUT` | MiMo 单轮超时，默认 900 秒 |
| `MIMO_EGRESS_AUDIT_PATH` | 可选的 Host 审计 JSONL 路径，用于证据收集 |

远程且启用认证的 CubeAPI 应使用 HTTPS。明文 HTTP 只适用于可信本地网络。

## 3. 运行参考模式

```bash
python speculative_mimo_code.py \
  --task fixtures/normalize-slug/task.json \
  --candidates 2 \
  --concurrency 2 \
  --evidence-file output/speculative-success.json
```

随附演示 fixture 包含尚未实现的 `normalize_slug` 与固定验收测试。
`task.json` 声明规划和实现说明、测试命令、可编辑 `app.py` 路径和候选策略。
短生命周期规划沙箱先运行禁止文件编辑与权限自动批准的 MiMo 父轮次，再只把
MiMo profile 复制到不带凭证的源 VM。候选 MicroVM 分叉这个导入会话。
`--concurrency` 必须不小于 `--candidates`，保证每个候选都会立即评估。

成功时输出：

```text
CUBE_MIMO_PROMOTION_OK
```

证据 JSON 记录有长度限制的执行元数据：沙箱与快照 ID、父/子 MiMo 会话 ID、
候选测试输出、改动路径与行数、错误、获胜者和最终结果。schema 不保存补丁
正文，但有界测试输出不可信，可能回显源码，因此分享前必须审查。收集器还会
单独确认其中不含真实密钥。

### 验证回滚路径

```bash
python speculative_mimo_code.py \
  --force-promotion-failure \
  --evidence-file output/speculative-rollback.json
```

该模式会在有效获胜补丁已应用后，仅强制最终源验证失败。驱动必须恢复干净的
源快照并输出：

```text
CUBE_MIMO_ROLLBACK_OK
```

## 用其他任务复用参考模式

复制 `fixtures/normalize-slug/`，只修改任务 profile 和项目：

```text
my-task/
├── task.json
└── project/
    ├── 源文件
    └── 固定验收测试
```

`task.json` 定义：

- `name` 和简短 `summary`；
- `planning_instructions` 与 `implementation_instructions`；
- 固定的 `test_command` 及其 `test_timeout_seconds`；
- 已存在文件组成的 `allowed_paths`；
- 具名候选 `strategies`；
- 表示初始测试结果的 `expect_baseline_failure`。

加载器会拒绝绝对/穿越路径、重复路径或策略、符号链接、超限 fixture，以及
基线中不存在的可编辑文件。双分叉生命周期、凭证边界、快照处理、选优、提升、
回滚、清理和证据格式保持不变。

这是后续应用的任务扩展接口。当前评估器刻意保持二元：固定测试通过或失败，
再按改动行数排序通过补丁。后续 MiMo Code `research-experiment` 集成可以新增
指标评估适配器，同时复用双分叉事务、凭证、回滚、清理和证据基础设施。

## 辅助预检

运行最小模板与 MiMo NDJSON 冒烟测试：

```bash
python run_mimo_code.py
```

运行默认拒绝出口与凭证边界预检：

```bash
python network_policy.py
```

这些只是辅助检查；本集成的主场景是推测式工作流。

## 验证

离线检查不需要模型 key 或 CubeSandbox 集群：

```bash
python -m unittest discover -s tests -v
python -m py_compile *.py tests/*.py \
  fixtures/normalize-slug/project/*.py \
  fixtures/normalize-slug/project/tests/*.py
bash -n build-template.sh collect_e2e_evidence.sh
```

在真实集群中，证据收集脚本会同时运行补丁提升和回滚场景，并检查本轮创建的
沙箱与快照 ID 最终都不存在：

```bash
./collect_e2e_evidence.sh
```

生成的证据位于 Git 已忽略的 `output/` 下。分享前仍应人工检查。

## 确定性选择

候选选择不会再请求另一个模型判断。候选只有同时满足以下条件才有资格：

- 分叉的 MiMo 会话与父会话不同；
- 能从对话中回忆连续性令牌；
- 固定验收测试通过，并在源沙箱原样重跑；
- 所有改动路径都由任务 `allowed_paths` 声明；
- 补丁非空、为文本且不超过配置的大小上限。

合格候选按 `(changed_lines, candidate_name)` 排序，使同一候选集合始终选出
相同结果，并把安全决策留在模型输出之外。

## 失败与清理语义

- 候选创建为全有或全无；任一创建失败会杀死已成功的兄弟沙箱。
- 单个候选失败不会丢弃其他有效候选。
- 没有合格候选时不会执行提升，源沙箱保持不变。
- 最终验证失败会原地回滚源沙箱。
- 持久基线快照会被显式删除；杀死源沙箱不会自动删除它。
- 若没有其他主错误，清理失败会使流程失败；若已有主错误，则输出带资源 ID
  的清理警告。

## 排错

| 现象 | 可能原因 | 处理 |
| --- | --- | --- |
| `mimo run` 没有 `--fork` | 模板或 CLI 过旧 | 重新构建固定版本模板 |
| MiMo Platform 返回 `401` / `403` | API key 缺失、过期或未正确注入 | 检查 `MIMO_API_KEY`、`api-key` 注入规则和脱敏 CubeEgress 审计 |
| 模板导入无法拉取镜像 | 镜像未推送、Cube 节点无仓库凭证或架构错误 | 推送 `linux/amd64` 镜像到所有 Cube 节点可访问的仓库，并配置仓库凭证 |
| 沙箱或 MiMo 命令超时 | 集群容量不足或任务超过限制 | 减少候选数，再按需增大 `MIMO_SANDBOX_TIMEOUT` 或 `MIMO_AGENT_EXEC_TIMEOUT` |
| 没有子会话 ID | MiMo CLI 事件契约变化 | 保留 `--format json` 并查看原始事件 |
| 连续性报告被拒绝 | 子会话未继承父上下文 | 检查快照时机与 `--session ... --fork` |
| 候选修改了禁止路径 | Agent 修改测试或任务策略之外的文件 | 收紧提示；只有文件确实需要编辑时才更新 `allowed_paths` |
| 没有合格候选 | 所有测试或补丁检查失败 | 查看各候选证据 |
| 提升失败 | 补丁漂移或源测试失败 | 驱动会自动回滚 |
| TLS 错误 | MiMo 不信任 CubeEgress CA | 正确设置 `MIMO_NODE_EXTRA_CA_CERTS` |
| `403` 或 curl 状态 `000` | 域名不匹配精确规则 | 使用 `api.xiaomimimo.com` 并检查审计日志 |
| 退出后仍有快照 | 清理请求失败 | 用记录的 snapshot/template ID 手动删除 |

## 参考资料

- [MiMo Code](https://github.com/XiaomiMiMo/MiMo-Code)
- [MiMo Code 会话](https://mimo.xiaomi.com/mimocode/sessions)
- [CubeSandbox snapshot、rollback 与 clone](../../docs/zh/guide/snapshot-rollback-clone.md)
- [CubeEgress 安全代理](../../docs/zh/guide/security-proxy.md)
- [文档站集成指南](../../docs/zh/guide/integrations/mimo-code.md)
