# 快速参考

## 部署命令

```bash
# 部署所有组件
kubectl apply -f deploy/kubernetes/

# 仅部署 webhook (假设 RBAC 已部署)
kubectl apply -f deploy/kubernetes/deployment.yaml \
            -f deploy/kubernetes/webhook.yaml
```

## 监控命令

```bash
# 查看 Pod 状态
kubectl get pods -n cube-system -l app=eviction-webhook

# 查看实时日志
kubectl logs -f -n cube-system -l app=eviction-webhook

# 查看 metrics
kubectl port-forward -n cube-system svc/eviction-webhook 8888:8888
curl http://localhost:8888/metrics

# 搜索特定事件
kubectl logs -n cube-system -l app=eviction-webhook | grep "eviction intercepted"
```

## 测试命令

```bash
# 单元测试
go test -v ./internal/...

# 集成测试
go test -v ./test/integration/...

# 所有测试
go test -v ./...

# 生成覆盖率报告
go test -cover ./...
```

## 故障排查命令

```bash
# 查看 webhook 配置
kubectl describe vwc eviction-webhook

# 查看 Pod 事件
kubectl describe pod -n cube-system -l app=eviction-webhook

# 查看恢复状态
kubectl exec -it -n cube-system eviction-webhook-xxx -- \
  cat /var/lib/eviction-webhook/recovery-state.json | jq .

# 查看审计日志（最后 10 行）
kubectl exec -it -n cube-system eviction-webhook-xxx -- \
  tail -10 /var/log/eviction-webhook/events.ndjson | jq .

# 检查 CubeMaster 连接
kubectl exec -it -n cube-system eviction-webhook-xxx -- \
  wget -O- http://cube-master.cube-system.svc:8089/healthz

# 重启 webhook
kubectl rollout restart deployment/eviction-webhook -n cube-system

# 查看重启历史
kubectl rollout history deployment/eviction-webhook -n cube-system

# 回滚到上一个版本
kubectl rollout undo deployment/eviction-webhook -n cube-system
```

## 关键指标

| 指标 | 含义 | 告警阈值 |
|------|------|---------|
| `eviction_webhook_intercepted_total` | 拦截驱逐次数 | > 100/min 异常高 |
| `eviction_webhook_recovery_duration_seconds` | 恢复延迟 | p95 > 10s |
| `eviction_webhook_cubemaster_api_latency_seconds` | API 调用延迟 | p95 > 5s |
| `eviction_webhook_cubemaster_errors_total` | API 错误次数 | > 10/min 异常 |
| `eviction_webhook_paused_sandboxes` | 当前暂停 sandbox 数 | > 100 时检查 |

## 日志格式

审计日志 (NDJSON 格式)：

```json
{"EventID":"uid-123","PodName":"sandbox-abc","Namespace":"cube-system","NodeName":"worker-01","InstanceType":"cubebox","InterceptedAt":"2026-08-03T10:00:00Z"}
```

结构化日志 (Zap 格式)：

```json
{"timestamp":"2026-08-03T10:00:00Z","level":"info","logger":"eviction-webhook","msg":"eviction intercepted","TraceID":"uid-123","PodName":"sandbox-abc"}
```

## 性能基准

- **Webhook 响应时间**: < 100ms (p99)
- **Recovery 时间**: < 30s (pause → resume cycle)
- **Metrics 查询**: < 50ms
- **Audit 日志写入**: < 10ms

## 清单

### 新部署检查清单

- [ ] TLS 证书已生成 (`kubectl get cert -n cube-system`)
- [ ] Secret 已创建 (`kubectl get secret eviction-webhook-auth -n cube-system`)
- [ ] RBAC 已部署 (`kubectl get role,rolebinding -n cube-system -l app=eviction-webhook`)
- [ ] Deployment 已运行 (`kubectl get deploy -n cube-system eviction-webhook`)
- [ ] ValidatingWebhookConfiguration 已创建 (`kubectl get vwc eviction-webhook`)
- [ ] Webhook 日志正常 (`kubectl logs -n cube-system -l app=eviction-webhook`)
- [ ] /metrics 端点可访问 (`curl :8888/metrics`)

### 故障排查检查清单

- [ ] Webhook Pod 运行中 (`kubectl get pods -n cube-system -l app=eviction-webhook`)
- [ ] CubeMaster 可达 (`kubectl exec ... wget -O- http://cube-master:8089/healthz`)
- [ ] TLS 证书未过期 (`kubectl get cert -n cube-system eviction-webhook-tls`)
- [ ] 最近没有错误 (`kubectl logs -n cube-system -l app=eviction-webhook | grep ERROR`)
- [ ] 恢复状态正常 (`kubectl exec ... cat /var/lib/eviction-webhook/recovery-state.json | jq .`)
