# Python 生产事故 RCA 代码解释器沙箱

本示例展示如何为 AI Agent Code Interpreter 场景构建并验证一个可复用的 CubeSandbox 模板。该模板提供 Python 数据科学运行时，支持多文件分析、有状态追问、快照 checkpoint/fork 和可下载产物。内置的生产事故 RCA 工作流是验证场景：宿主机上传服务指标、部署事件、告警和 runbook，沙箱执行 Python 分析、为中间工作区创建 checkpoint、从 checkpoint fork 出新沙箱继续分析，最终生成中文 RCA 报告、图表、结构化摘要和可下载结果包。

## 功能亮点

1. **工业数据分析工作流**：使用 Pandas 分析服务指标、SLO 阈值、部署事件和告警时间线，定位异常窗口和可疑触发因素。
2. **有状态代码解释器工作区**：第一轮分析在 `/tmp/cubesandbox-incident-rca/state` 写入中间状态。
3. **快照 checkpoint 与 fork**：客户端为第一轮工作区创建 snapshot，从该 checkpoint 启动一个新沙箱，验证状态继承后在 fork 沙箱中执行第二轮分析。
4. **Matplotlib 中文渲染**：镜像内安装 `fonts-wqy-zenhei`，分析脚本配置 Matplotlib 使用 `WenQuanYi Zen Hei`，避免中文标题和坐标轴显示成方块。
5. **动态安装第三方包**：客户端在运行中的沙箱里安装锁定版本的 `emoji` 和 `humanize`，随后在第二轮分析中立即导入并参与报告生成。
6. **多文件输入和打包输出**：沙箱生成中文事故图、Markdown RCA 报告、SLO 汇总 CSV、部署关联 JSON、manifest JSON，并在沙箱内打成 tarball 供宿主机下载。

## 可复用模板模式

RCA 场景是一个具体示例，但模板模式可以复用于其他代码解释器任务：

- **输入**：宿主机或 Agent 上传 CSV、JSON、日志、runbook 或生成的分析程序。
- **工作区状态**：中间文件统一写入沙箱内可预测的 `state/` 目录。
- **Checkpoint/fork**：snapshot 捕获数据摄入后的工作区，后续可以从该状态继续分析、重试不同分支，或保留审计点。
- **产物**：报告、图表、结构化摘要、manifest 和必要的中间状态会被打包为 tarball 下载。

它与仓库中已有的 OpenAI Agents code interpreter 示例不同：本示例聚焦可复用的 CubeSandbox 模板和生产风格 RCA 工作流，而不仅是 SDK 集成方式。

## 受限出口或离线部署

运行时执行 `pip3 install emoji humanize` 是为了演示交互式代码解释器模式：Agent 可以在分析过程中追加轻量依赖。生产环境、受限出口环境或完全离线部署中，可以把这些包预装进 Dockerfile，或通过内网 PyPI 镜像提供依赖。依赖进入模板镜像后，核心 RCA 流程不需要不受限制的公网访问。

## 文件说明

- `Dockerfile`：构建可复用的 Python 数据科学沙箱镜像。
- `incident_metrics.csv`：`checkout-api` 的时间序列服务指标。
- `deployments.json`：用于关联分析的部署事件。
- `alerts.csv`：从监控系统导出的告警时间线。
- `runbook.json`：SLO 阈值和事故处理策略提示。
- `round1_detect.py`：上传到沙箱中的第一轮异常检测程序。
- `round2_rca.py`：上传到 fork 沙箱中的第二轮 RCA 与报告打包程序。
- `test_data_science.py`：端到端客户端，负责创建沙箱、上传文件、执行两轮分析、下载结果包，并验证 manifest。
- `test_data_science_unit.py`：覆盖沙箱 ID 解析与安全归档解压的离线测试。
- `env.example`：本地 CubeSandbox API/proxy 配置和模板 ID 占位符。

## 步骤 1：构建镜像

请在 Cube 节点运行时能够访问到该镜像的位置构建：

```bash
docker build -t cubesandbox-data-science:latest .
```

该镜像包含 Python、Pandas、Matplotlib、科学计算依赖和中文字体。由于面向代码解释器类负载，它会比最小镜像更大。

## 步骤 2：注册 CubeSandbox 模板

进入 CubeSandbox 开发虚拟机，或在已安装 `cubemastercli` 的节点上执行：

```bash
cubemastercli tpl create-from-image \
    --image               cubesandbox-data-science:latest \
    --writable-layer-size 2G \
    --expose-port         49983 \
    --probe               49983 \
    --probe-path          /health
```

记录命令返回的模板 ID，例如 `tpl-xxxxxxxxxxxxxxxxxxxxxxxx`。

## 步骤 3：配置客户端

安装客户端依赖：

```bash
pip3 install -r requirements.txt
```

连接集群前可以先运行聚焦离线测试：

```bash
pytest -q test_data_science_unit.py
```

复制 `.env` 并填入模板 ID：

```bash
cp env.example .env
```

本地 dev VM 场景下，`.env` 应类似：

```bash
E2B_API_URL="http://127.0.0.1:13000"
CUBE_REMOTE_PROXY_BASE="https://127.0.0.1:11443"
CUBE_TEMPLATE_ID="tpl-xxxxxxxxxxxxxxxxxxxxxxxx"
E2B_API_KEY=e2b_dummyapikeyforlocaltest
```

## 步骤 4：运行端到端示例

```bash
python3 test_data_science.py
```

脚本会依次完成：

1. 使用已注册模板启动沙箱。
2. 上传 `incident_metrics.csv`、`deployments.json`、`alerts.csv`、`runbook.json` 和两段生成的分析脚本。
3. 在沙箱内动态安装 `emoji` 和 `humanize`。
4. 执行第一轮异常检测，并把中间状态保存到沙箱工作区。
5. 基于第一轮工作区创建 CubeSandbox snapshot checkpoint。
6. 从 checkpoint 启动一个新沙箱，并验证中间状态文件已继承。
7. 在 fork 沙箱中执行第二轮 RCA 分析，复用第一轮状态、部署事件、告警和 runbook 策略。
8. 在 fork 沙箱内创建 `/tmp/cubesandbox-incident-rca-results.tar.gz`。
9. 下载并解压结果包到 `output/results/`。

预期解压结果：

- `事故分析图.png`：中文事故分析图，用于验证字体渲染正常。
- `incident_report.md`：Markdown RCA 报告，包含可疑触发因素和建议动作。
- `slo_summary.csv`：基线和事故峰值的 SLO 指标对比。
- `deployment_correlation.json`：部署事件与事故窗口的结构化关联结果。
- `manifest.json`：机器可读的产物清单和关键指标。
- `state/anomaly_windows.csv`、`state/baseline.json` 与 `state/checkpoint_snapshot_id.txt`：第一轮分析生成的中间状态和第二轮 fork 所用 checkpoint，随结果包一起归档，便于审计。
