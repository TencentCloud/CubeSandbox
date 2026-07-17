# 崩溃恢复 Worker

[English](README.md)

本示例演示一个长期运行的有状态 Worker 如何利用 CubeSandbox checkpoint
实现断点续跑。固定 seed 的 workload 会让同一个 Worker 连续运行三个 epoch；
每个 epoch 都会创建 checkpoint、继续提交新任务、在下一笔转账中途主动终止
Worker、回滚同一个 sandbox、重放丢失任务，然后从恢复后的状态继续运行。

CubeSandbox 并不会自动赋予应用事务原子性。应用仍然需要在稳定边界创建
checkpoint；当后续执行被破坏时，CubeSandbox 负责恢复快照中捕获的进程内存和
可写文件系统。

## 场景

Worker 在内存中维护：

- 初始余额与当前余额；
- pending transfer；
- 已提交 ledger；
- 用于幂等处理的 seen request IDs；
- committed、duplicate 和 fault 统计值。

Worker 还会将状态转换镜像到
`/workspace/crash-recovery/audit.jsonl`。每次请求完成或触发故障之前，
都会通过原子文件替换将 audit 落盘。

默认 workload 包含三个 epoch，每个 epoch 的执行流程如下：

```text
固定 seed 转账 x4 -- 每笔验证 --> checkpoint Cn
                                         |
                                         v
                             再提交转账 x2 -- 每笔验证
                                         |
                                         v
                           下一笔转账 debit 后主动 abort
                                         |
                                         v
                                验证未完成的持久化 audit
                                         |
                                         v
                                  rollback(Cn)
                                         |
                                         v
                            验证内存与工作区精确恢复
                                         |
                                         v
                         重放丢失转账并重试故障转账
                                         |
                                         v
                              验证幂等处理，进入下一轮
```

转账由固定 seed 的伪随机生成器产生。生成器会在宿主机维护余额模型，并且只生成
来源账户余额足够支付的合法转账。checkpoint 后的两笔转账会在崩溃前正常提交，
但在 rollback 时消失；驱动随后使用相同 request ID 重新提交它们，以展示断点续跑。

## 验证的不变量

`run_demo.py` 会独立验证：

- 每次成功提交或幂等去重后，Worker 状态都与宿主机 reference model 精确一致；
- 账户总余额不变；
- 从初始余额重放 ledger 后得到当前余额；
- ledger 中的 request ID 唯一；
- `seen` 与已提交 ledger 的 ID 集合相同；
- pending 与 committed 集合不相交；
- committed 数量等于 ledger 长度；
- audit 中的初始余额与内存状态相同；
- audit 中的 committed transfer 与内存 ledger 相同；
- 稳定状态不存在未完成或 fault audit；
- 每次 rollback 都会恢复 checkpoint 的完整状态以及字节级相同的 audit 文件；
- checkpoint 后的 request ID 会在 rollback 后消失，并在重放时重新提交成功；
- 重复重试不会再次修改余额或 ledger；
- 最终 committed、duplicate 和 fault 统计值与完整的多 epoch workload 一致。

每次 rollback 之前，驱动还会验证当前 epoch 的故障转账存在 `started` 和
`fault_injected` 记录但不存在 `committed` 记录，之前的完整 committed 历史仍然
存在，并且故障记录中的余额总和恰好减少了已经扣除的金额。

## 文件

| 文件 | 用途 |
| --- | --- |
| `src/domain.rs` | 转账状态机和原子 audit 持久化 |
| `src/http.rs` | HTTP API 和故障终止器 |
| `src/lib.rs` | 公共模块导出 |
| `src/main.rs` | Worker 进程入口 |
| `run_demo.py` | CubeSandbox 生命周期控制与独立一致性验证 |
| `test_run_demo.py` | 宿主机侧不变量测试 |
| `tests/` | Rust 领域、HTTP、二进制和进程终止测试 |
| `Dockerfile` | CubeSandbox 模板的多阶段镜像构建 |

## 本地构建与测试

```bash
cargo test
python3 -m unittest -v test_run_demo.py
docker build -t cubesandbox-crash-recovery-worker:latest .
```

可以在普通 Docker 中进行 Worker smoke test：

```bash
docker run --rm -d \
  --name cubesandbox-crash-recovery-worker \
  -p 18080:8080 \
  cubesandbox-crash-recovery-worker:latest \
  /usr/local/bin/crash-recovery-worker

curl http://127.0.0.1:18080/health
curl http://127.0.0.1:18080/state

docker rm -f cubesandbox-crash-recovery-worker
```

## 注册模板

将镜像推送到 CubeSandbox 节点可以访问的仓库，然后执行：

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/cubesandbox-crash-recovery-worker:latest \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --expose-port 8080 \
  --probe 49983 \
  --probe-path /health
```

readiness probe 指向 `49983` 端口上的 `envd`。`run_demo.py` 会在创建
sandbox 后将 Worker 作为子进程启动，因此 Worker abort 不会同时终止
`envd`，也不会在 rollback 前销毁 sandbox。

驱动还会关闭 sandbox 的公开流量，并在 Worker 请求中自动携带该 sandbox 的
traffic token，因此故障注入 endpoint 无法通过 CubeProxy 被匿名访问。

## 在 CubeSandbox 上运行

```bash
python3 -m venv .venv
. .venv/bin/activate
pip install -r requirements.txt

cp .env.example .env
# 填写 CUBE_TEMPLATE_ID，并按本地部署调整其他配置。

python3 run_demo.py
```

如果运行驱动的机器无法解析 sandbox wildcard domain，可以设置
`CUBE_PROXY_NODE_IP`，并按需设置 `CUBE_PROXY_PORT_HTTP`。Worker 请求会直接连接
该 HTTP endpoint，同时通过 `Host` header 保留虚拟 sandbox hostname，供
CubeProxy 完成路由。

可以通过参数延长 workload，而不需要修改代码：

```bash
python3 run_demo.py \
  --cycles 5 \
  --transfers-before-checkpoint 8 \
  --transfers-after-checkpoint 3 \
  --seed 42
```

| 参数 | 默认值 | 用途 |
| --- | ---: | --- |
| `--cycles` | `3` | checkpoint、故障、rollback 和重放的 epoch 数量 |
| `--transfers-before-checkpoint` | `4` | 每次 checkpoint 前持续积累并验证的转账数量 |
| `--transfers-after-checkpoint` | `2` | rollback 时故意丢失、随后重新执行的成功转账数量 |
| `--seed` | `20260717` | 可复现合法转账生成器使用的 seed |

预期的最后几行输出：

```text
[epoch 3/3] Rollback verified: snapshot=... discarded=2 state=... audit=...
[epoch 3/3] Fault retry verified: id=tx-fault-03 ledger=21 state=...
[epoch 3/3] Duplicate verified: id=tx-fault-03 duplicates=3
[epoch 3/3] PASS: committed=21 state=...
[summary] PASS: cycles=3 checkpoints=3 committed=21 duplicates=3 seed=20260717
```

## HTTP API

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/health` | Worker readiness |
| `GET` | `/state` | 验证器使用的完整内存状态 |
| `POST` | `/transfers` | 提交 transfer 或执行幂等去重 |

transfer body：

```json
{
  "id": "tx-001",
  "from": "alice",
  "to": "bob",
  "amount": 100
}
```

## 范围与限制

rollback 只能恢复 sandbox 内部捕获的状态，包括进程内存和可写文件系统；它不能
撤销已经发送到外部数据库、远程服务、消息队列或 TCP 对端的副作用。rollback
之后原有 TCP 连接也已经失效，因此驱动会创建新的 HTTP session，再连接恢复后的
Worker。
