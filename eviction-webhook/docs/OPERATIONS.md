# eviction-webhook 运维指南

## 部署后验证

### 1. 检查 Pod 状态
```bash
kubectl get pods -n cube-system -l app=eviction-webhook
kubectl describe pod -n cube-system -l app=eviction-webhook
```

### 2. 检查 ValidatingWebhookConfiguration
```bash
kubectl get validatingwebhookconfigurations
kubectl describe vwc eviction-webhook
```

### 3. 监控日志
```bash
# 查看实时日志
kubectl logs -f -n cube-system -l app=eviction-webhook

# 查看特定事件
kubectl logs -n cube-system -l app=eviction-webhook | grep "eviction intercepted"
```

## 性能指标

### 监控关键指标

```bash
# Port-forward metrics 端口
kubectl port-forward -n cube-system svc/eviction-webhook 8888:8888

# 查看 Prometheus metrics
curl http://localhost:8888/metrics | grep eviction_webhook
```

**关键指标**:
- `eviction_webhook_intercepted_total` — 拦截驱逐次数
- `eviction_webhook_recovery_duration_seconds_bucket` — 恢复延迟分布
- `eviction_webhook_cubemaster_api_latency_seconds` — API 调用延迟
- `eviction_webhook_paused_sandboxes` — 当前暂停 sandbox 数量

### 告警规则示例

```yaml
# Prometheus alerts
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
      summary: "High eviction processing failure rate"

  - alert: HighRecoveryLatency
    expr: |
      histogram_quantile(0.95, eviction_webhook_recovery_duration_seconds_bucket) > 10
    for: 5m
    annotations:
      summary: "High sandbox recovery latency (p95 > 10s)"
```

## 故障排查

### 问题：驱逐仍然发生

**症状**: Sandbox 仍然被驱逐，恢复未触发

**诊断步骤**:
1. 检查 webhook 是否部署
   ```bash
   kubectl get deploy -n cube-system eviction-webhook
   ```

2. 检查 webhook 配置是否正确
   ```bash
   kubectl get vwc eviction-webhook -o yaml
   ```

3. 检查 TLS 证书是否有效
   ```bash
   kubectl get cert -n cube-system eviction-webhook-tls
   ```

4. 查看 webhook 日志中的错误
   ```bash
   kubectl logs -n cube-system -l app=eviction-webhook | grep -i error
   ```

**解决方案**:
- 重新生成证书: `kubectl delete cert eviction-webhook-tls -n cube-system && kubectl apply -f deploy/kubernetes/cert.yaml`
- 重启 webhook: `kubectl rollout restart deployment/eviction-webhook -n cube-system`

### 问题：Sandbox 未恢复

**症状**: Sandbox 被暂停但未在压力解除后恢复

**诊断步骤**:
1. 检查 CubeMaster 连接
   ```bash
   kubectl exec -it -n cube-system eviction-webhook-xxx -- \
     wget -O- http://cube-master.cube-system.svc:8089/healthz
   ```

2. 查看恢复状态
   ```bash
   kubectl exec -it -n cube-system eviction-webhook-xxx -- \
     cat /var/lib/eviction-webhook/recovery-state.json | jq .
   ```

3. 检查审计日志
   ```bash
   kubectl exec -it -n cube-system eviction-webhook-xxx -- \
     tail -f /var/log/eviction-webhook/events.ndjson
   ```

**解决方案**:
- 手动恢复 sandbox (如果 CubeMaster 不可达)
  ```bash
  curl -X POST http://cubemaster:8089/cube/sandbox/update \
    -H "Content-Type: application/json" \
    -d '{
      "sandbox_id": "xxx",
      "instance_type": "cubebox",
      "action": "resume"
    }'
  ```

- 重启 webhook (会从磁盘加载恢复状态)
  ```bash
  kubectl rollout restart deployment/eviction-webhook -n cube-system
  ```

## 零停机升级

```bash
# 1. 构建新镜像
docker build -t eviction-webhook:v2.0.0 .

# 2. 推送到 registry
docker push eviction-webhook:v2.0.0

# 3. 更新 Deployment (自动滚动更新)
kubectl set image deployment/eviction-webhook \
  eviction-webhook=eviction-webhook:v2.0.0 \
  -n cube-system

# 4. 验证升级
kubectl rollout status deployment/eviction-webhook -n cube-system

# 5. 如需回滚
kubectl rollout undo deployment/eviction-webhook -n cube-system
```

## 备份和恢复

### 备份恢复状态

```bash
kubectl exec -n cube-system eviction-webhook-xxx -- \
  cat /var/lib/eviction-webhook/recovery-state.json > recovery-state-backup.json

kubectl exec -n cube-system eviction-webhook-xxx -- \
  cat /var/log/eviction-webhook/events.ndjson > audit-log-backup.ndjson
```

### 恢复恢复状态

```bash
kubectl cp recovery-state-backup.json \
  cube-system/eviction-webhook-xxx:/var/lib/eviction-webhook/recovery-state.json
```

## 性能优化

### 调整副本数

```bash
# 增加副本数以提高可用性
kubectl scale deployment/eviction-webhook \
  --replicas=3 \
  -n cube-system
```

### 调整资源限制

```yaml
# 在 deployment.yaml 中修改
resources:
  requests:
    cpu: 100m
    memory: 128Mi
  limits:
    cpu: 500m
    memory: 512Mi
```

## 常见命令

```bash
# 查看日志
kubectl logs -f -n cube-system -l app=eviction-webhook

# 进入 Pod
kubectl exec -it -n cube-system eviction-webhook-xxx -- /bin/sh

# 查看 metrics
kubectl port-forward -n cube-system svc/eviction-webhook 8888:8888

# 查看审计日志
kubectl exec -it -n cube-system eviction-webhook-xxx -- \
  tail -f /var/log/eviction-webhook/events.ndjson | jq .

# 查看恢复状态
kubectl exec -it -n cube-system eviction-webhook-xxx -- \
  cat /var/lib/eviction-webhook/recovery-state.json | jq .

# 强制清空恢复状态（谨慎！）
kubectl exec -it -n cube-system eviction-webhook-xxx -- \
  sh -c 'echo "{}" > /var/lib/eviction-webhook/recovery-state.json'

# 重启 webhook
kubectl rollout restart deployment/eviction-webhook -n cube-system
```
