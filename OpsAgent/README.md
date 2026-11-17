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
| 监听端口 | `8890`（`listen_addr`，默认 `0.0.0.0:8890`） |
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
- `:8890`（push）与 `:3010`（cubelet 注册/心跳）、pull reconcile 同属内网边界信任模型，均不做身份鉴权。
- 真实安全边界是配额写入前的超卖上限校验（CPU 20x / 内存 3x），push、pull 两条路径均生效。
- 每次写盘前备份（轮转保留 5 份）。

## 构建运行

```sh
go build -o bin/ops-agent ./cmd/ops-agent
./bin/ops-agent --config config/config.example.yaml
```
