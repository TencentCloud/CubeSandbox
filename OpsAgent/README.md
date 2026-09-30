# ops-agent

ops-agent 是 CubeSandbox 在每台节点上部署的本地常驻 Agent 守护进程，属于控制面（CubeOps）与数据面（Cubelet）之间的节点侧执行/管控代理。

> 承接 CubeOps 下发的管控指令，负责本机受控配置的落地与一致性收敛。自身无状态。

## 功能

**quota 配置**：承接 CubeOps 的节点配额下发，写入 `/usr/local/services/cubetoolbox/Cubelet/dynamicconf/conf.yaml`，并对账收敛漂移。

- **Push**：CubeOps 每次变更后下发配额 spec，agent 校验后合并进 cubelet dynamicconf，同步应答。
- **Pull**：每个 reconcile tick（默认 300s）拉取权威 spec 纠正漂移；文件未变则零写入，漂移（含手改）则重写并轮转备份。
- 写入采用原子替换，避免写坏配置文件（tmp → fsync → rename，写后读回校验）。

## 部署

| 项 | 值 |
|---|---|
| 安装目录 | `/usr/local/services/cubetoolbox/ops-agent` |
| 配置 | `conf/config.yaml`（见 `config/config.example.yaml`） |
| 监听端口 | `8890`（`listen_addr`，默认 `127.0.0.1:8890`） |
| 托管 | systemd，单元 `cube-sandbox-ops-agent.service` |
| 日志 | `/data/log/ops-agent/ops-agent-req.log` |

## 状态查询

```sh
# 服务状态
systemctl status cube-sandbox-ops-agent

# 存活探针（纯 liveness，返回 {"ok":true}）
curl -s http://127.0.0.1:8890/health

# 版本
/usr/local/services/cubetoolbox/ops-agent/bin/ops-agent -v
```

## 安全模型

- 白名单动作，不执行命令。
- **push 鉴权**：`POST /api/v1/config/quota` 要求携带 `X-Ops-Agent-Token` header，与配置的 `shared_token` 一致才放行。
- **fail-closed**：`shared_token` 未配置时拒绝所有 push。
- `/health` 探针不鉴权。
- 每次写盘前备份（轮转保留 5 份）。

### 鉴权配置

| 配置项 | 环境变量 | 说明 |
|---|---|---|
| `shared_token` | `OPS_AGENT_SHARED_TOKEN` | 与 CubeOps 的 `CUBE_OPS_OPSAGENT_TOKEN` 保持一致 |

CubeOps 侧 push 时自动携带该 token；两者由部署链路（one-click / k8s / terraform）生成并注入同一个值。token 不匹配或缺失的 push 返回 401，由 pull reconcile（300s）兜底，不影响最终一致性。

**one-click 分离部署（split）**：token 由控制面节点生成，计算节点不会自动生成，需从控制面节点拷贝同一值到计算节点的 `.env`：

```sh
# 在控制面节点执行，把输出拷到计算节点的 .env
grep '^CUBE_OPS_OPSAGENT_TOKEN=' /usr/local/services/cubetoolbox/.one-click.env
```

计算节点缺失该 token 时，ops-agent 的 push 鉴权 fail-closed（拒绝所有 push，由 300s pull reconcile 兜底收敛）。

## 构建运行

```sh
go build -o bin/ops-agent ./cmd/ops-agent
./bin/ops-agent --config config/config.example.yaml
```
