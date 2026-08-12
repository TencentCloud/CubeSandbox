---
title: 驱逐保护（eviction-webhook）
lang: zh-CN
---

# 驱逐保护（eviction-webhook）

`eviction-webhook` 是一个可选的 `ValidatingWebhook`，用于防止 sandbox MicroVM 在节点内存压力下被 kubelet
自动驱逐直接销毁。它会拒绝驱逐请求，隔离（cordon）压力节点，通过 CubeMaster 暂停 sandbox，并在节点
`MemoryPressure` 解除后自动恢复。

::: tip 如何开启
`eviction-webhook` 作为顶层 Helm chart 的可选组件下发，**默认关闭**。通过
`--set evictionWebhook.enabled=true` 开启。TLS、鉴权、`recoveryEnabled`（只拒绝驱逐但不自动
隔离/暂停）等配置见
[`values.yaml`](https://github.com/TencentCloud/CubeSandbox/blob/master/deploy/kubernetes/chart/values.yaml)。
:::

## 读完本页你会知道

- 如何确认 webhook 已正确部署并生效拦截驱逐
- 需要关注哪些 Prometheus 指标和告警规则
- 驱逐/恢复失败时如何排查
- 如何零停机升级以及备份恢复状态

## 部署后验证

```bash
# Pod 与 ValidatingWebhookConfiguration
kubectl get pods -n cube-system -l app=eviction-webhook
kubectl get validatingwebhookconfigurations eviction-webhook

# 实时日志，筛选拦截事件
kubectl logs -f -n cube-system -l app=eviction-webhook
kubectl logs -n cube-system -l app=eviction-webhook | grep "eviction intercepted"
```

::: tip 新部署检查清单
- [ ] TLS 证书已生成（`kubectl get cert -n cube-system`）
- [ ] 鉴权 Secret 已创建（`kubectl get secret eviction-webhook-auth -n cube-system`）
- [ ] RBAC 已部署（`kubectl get clusterrole,clusterrolebinding -l app.kubernetes.io/component=eviction-webhook`）
- [ ] Deployment 已就绪（`kubectl get deploy -n cube-system eviction-webhook`）
- [ ] `ValidatingWebhookConfiguration` 已创建（`kubectl get vwc eviction-webhook`）
- [ ] `/metrics` 端点可访问（`curl :8888/metrics`）
:::

## 监控指标

Metrics 通过独立的非 TLS 端口暴露（默认 `:8888`）：

```bash
kubectl port-forward -n cube-system svc/eviction-webhook 8888:8888
curl http://localhost:8888/metrics | grep eviction_webhook
```

| 指标 | 类型 | 说明 |
|------|------|------|
| `eviction_webhook_intercepted_total` | Counter | 拦截驱逐次数，按 node / instance_type / reason 维度 |
| `eviction_webhook_requests_total` | Counter | Webhook 请求总数，按 operation / allowed 维度 |
| `eviction_webhook_request_latency_seconds` | Histogram | Webhook 请求延迟 |
| `eviction_webhook_recovery_duration_seconds` | Histogram | Sandbox 恢复耗时（pause → resume） |
| `eviction_webhook_cubemaster_api_latency_seconds` | Histogram | CubeMaster API 调用延迟 |
| `eviction_webhook_cubemaster_errors_total` | Counter | CubeMaster API 错误次数 |
| `eviction_webhook_isolated_nodes_total` | Counter | 已隔离节点总数 |

告警规则示例：

```yaml
groups:
- name: eviction-webhook
  rules:
  - alert: HighEvictionFailureRate
    expr: |
      rate(eviction_webhook_cubemaster_errors_total[5m])
      /
      rate(eviction_webhook_intercepted_total[5m]) > 0.1
    for: 5m
    annotations:
      summary: "驱逐处理失败率过高"
  - alert: HighRecoveryLatency
    expr: |
      histogram_quantile(0.95, eviction_webhook_recovery_duration_seconds_bucket) > 10
    for: 5m
    annotations:
      summary: "Sandbox 恢复延迟过高（p95 > 10s）"
```

## 故障排查

### 驱逐仍然发生（sandbox 被销毁，未触发恢复）

1. 确认 webhook 已部署，且 `ValidatingWebhookConfiguration` 能匹配到 sandbox Pod：
   ```bash
   kubectl get deploy -n cube-system eviction-webhook
   kubectl get vwc eviction-webhook -o yaml
   ```
2. 检查 TLS 证书是否有效（证书过期/无效 + `failurePolicy: Ignore` 会导致 API Server 静默跳过该
   webhook，驱逐请求会直接放行）：
   ```bash
   kubectl get cert -n cube-system eviction-webhook-tls
   ```
3. 查看 webhook 日志中的错误：
   ```bash
   kubectl logs -n cube-system -l app=eviction-webhook | grep -i error
   ```

**解决方案**：重新生成证书（`kubectl delete cert eviction-webhook-tls -n cube-system` 后重新
apply/upgrade release）或重启 webhook（`kubectl rollout restart deployment/eviction-webhook -n cube-system`）。

### Sandbox 已暂停但未恢复

1. 确认 webhook Pod 能连通 CubeMaster：
   ```bash
   kubectl exec -it -n cube-system <eviction-webhook-pod> -- \
     wget -O- http://cube-master.cube-system.svc:8089/healthz
   ```
2. 查看持久化的恢复状态和审计日志：
   ```bash
   kubectl exec -it -n cube-system <eviction-webhook-pod> -- \
     cat /var/lib/eviction-webhook/recovery-state.json | jq .
   kubectl exec -it -n cube-system <eviction-webhook-pod> -- \
     tail -f /var/log/eviction-webhook/events.ndjson
   ```

**解决方案**：若 CubeMaster 暂不可达，可在其恢复后手动 resume：
```bash
curl -X POST http://cube-master:8089/cube/sandbox/update \
  -H "Content-Type: application/json" \
  -d '{"sandbox_id": "<id>", "instance_type": "cubebox", "action": "resume"}'
```
重启 webhook 也会基于磁盘上的持久化状态重新触发一次协调（reconcile）。

## 零停机升级

Chart 管理的 Deployment 使用 `Recreate` 策略（recovery-state PVC 是 `ReadWriteOnce`，新 Pod
挂载前必须先删除旧 Pod）；`failurePolicy: Ignore` 保证了这段短暂间隙是安全的——驱逐请求会被
放行而不是被卡住。

```bash
helm upgrade cube deploy/kubernetes/chart --namespace cube-system \
  --set evictionWebhook.enabled=true \
  --set images.evictionWebhook.tag=v2.0.0

kubectl rollout status deployment/eviction-webhook -n cube-system
# 如需回滚：
kubectl rollout undo deployment/eviction-webhook -n cube-system
```

## 备份与恢复恢复状态

```bash
# 备份
kubectl exec -n cube-system <eviction-webhook-pod> -- \
  cat /var/lib/eviction-webhook/recovery-state.json > recovery-state-backup.json

# 恢复
kubectl cp recovery-state-backup.json \
  cube-system/<eviction-webhook-pod>:/var/lib/eviction-webhook/recovery-state.json
```

## 常用命令

```bash
# 调整副本数
kubectl scale deployment/eviction-webhook --replicas=3 -n cube-system

# 查看恢复状态 / 审计日志
kubectl exec -it -n cube-system <eviction-webhook-pod> -- \
  cat /var/lib/eviction-webhook/recovery-state.json | jq .
kubectl exec -it -n cube-system <eviction-webhook-pod> -- \
  tail -10 /var/log/eviction-webhook/events.ndjson | jq .

# 强制清空恢复状态（谨慎操作——已记录为 paused 的 sandbox 将不再被自动恢复追踪）
kubectl exec -it -n cube-system <eviction-webhook-pod> -- \
  sh -c 'echo "{}" > /var/lib/eviction-webhook/recovery-state.json'
```
