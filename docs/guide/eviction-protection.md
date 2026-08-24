---
title: Eviction Protection
lang: en-US
---

# Eviction Protection

`eviction-webhook` is an optional `ValidatingWebhook` that protects sandbox MicroVMs from being
destroyed by kubelet's automatic Pod eviction under node memory pressure. Instead of letting the
Pod be evicted (which destroys the MicroVM), it denies the eviction, cordons the pressured node,
pauses the sandbox via CubeMaster, and resumes it automatically once `MemoryPressure` clears.

::: tip Enabling it
`eviction-webhook` ships as part of the top-level Helm chart and is **disabled by default**.
Enable it with `--set evictionWebhook.enabled=true`. See
[`values.yaml`](https://github.com/TencentCloud/CubeSandbox/blob/master/deploy/kubernetes/chart/values.yaml)
for TLS, auth, and `recoveryEnabled` (deny eviction without the automatic cordon/pause side effect).
:::

## What you'll learn

- How to verify the webhook is deployed and intercepting evictions correctly
- Which Prometheus metrics and alert rules to watch
- How to troubleshoot eviction and recovery failures
- How to perform zero-downtime upgrades and back up recovery state

## Verifying the deployment

```bash
# Pod and ValidatingWebhookConfiguration
kubectl get pods -n cube-system -l app=eviction-webhook
kubectl get validatingwebhookconfigurations eviction-webhook

# Live logs, filtered to interception events
kubectl logs -f -n cube-system -l app=eviction-webhook
kubectl logs -n cube-system -l app=eviction-webhook | grep "eviction intercepted"
```

::: tip New deployment checklist
- [ ] TLS certificate issued (`kubectl get cert -n cube-system`)
- [ ] Auth secret created (`kubectl get secret eviction-webhook-auth -n cube-system`)
- [ ] RBAC applied (`kubectl get clusterrole,clusterrolebinding -l app.kubernetes.io/component=eviction-webhook`)
- [ ] Deployment ready (`kubectl get deploy -n cube-system eviction-webhook`)
- [ ] `ValidatingWebhookConfiguration` present (`kubectl get vwc eviction-webhook`)
- [ ] `/metrics` reachable (`curl :8888/metrics`)
:::

## Metrics

Metrics are served on a separate, non-TLS port (`:8888` by default):

```bash
kubectl port-forward -n cube-system svc/eviction-webhook 8888:8888
curl http://localhost:8888/metrics | grep eviction_webhook
```

| Metric | Type | Description |
|--------|------|-------------|
| `eviction_webhook_intercepted_total` | Counter | Evictions intercepted, by node / instance\_type / reason |
| `eviction_webhook_requests_total` | Counter | Total webhook requests, by operation / allowed |
| `eviction_webhook_request_latency_seconds` | Histogram | Webhook request latency |
| `eviction_webhook_recovery_duration_seconds` | Histogram | Sandbox recovery duration (pause → resume) |
| `eviction_webhook_cubemaster_api_latency_seconds` | Histogram | CubeMaster API call latency |
| `eviction_webhook_cubemaster_errors_total` | Counter | CubeMaster API errors |
| `eviction_webhook_isolated_nodes_total` | Counter | Total nodes isolated |

Example alert rules:

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
      summary: "High eviction processing failure rate"
  - alert: HighRecoveryLatency
    expr: |
      histogram_quantile(0.95, eviction_webhook_recovery_duration_seconds_bucket) > 10
    for: 5m
    annotations:
      summary: "High sandbox recovery latency (p95 > 10s)"
```

## Troubleshooting

### Eviction still happens (sandbox destroyed, no recovery)

1. Confirm the webhook is deployed and the `ValidatingWebhookConfiguration` matches sandbox Pods:
   ```bash
   kubectl get deploy -n cube-system eviction-webhook
   kubectl get vwc eviction-webhook -o yaml
   ```
2. Check the TLS certificate is valid (an expired/invalid cert + `failurePolicy: Ignore` means the
   API server silently skips the webhook and evictions go through unopposed):
   ```bash
   kubectl get cert -n cube-system eviction-webhook-tls
   ```
3. Look for errors in the webhook logs:
   ```bash
   kubectl logs -n cube-system -l app=eviction-webhook | grep -i error
   ```

**Fixes:** regenerate the certificate (`kubectl delete cert eviction-webhook-tls -n cube-system`,
then re-apply/upgrade the release) or restart the webhook
(`kubectl rollout restart deployment/eviction-webhook -n cube-system`).

### Sandbox paused but never resumed

1. Confirm CubeMaster is reachable from the webhook Pod:
   ```bash
   kubectl exec -it -n cube-system <eviction-webhook-pod> -- \
     wget -O- http://cube-master.cube-system.svc:8089/healthz
   ```
2. Inspect the persisted recovery state and audit log:
   ```bash
   kubectl exec -it -n cube-system <eviction-webhook-pod> -- \
     cat /var/lib/eviction-webhook/recovery-state.json | jq .
   kubectl exec -it -n cube-system <eviction-webhook-pod> -- \
     tail -f /var/log/eviction-webhook/events.ndjson
   ```

**Fixes:** if CubeMaster is unreachable, resume the sandbox manually once it recovers:
```bash
curl -X POST http://cube-master:8089/cube/sandbox/update \
  -H "Content-Type: application/json" \
  -d '{"sandbox_id": "<id>", "instance_type": "cubebox", "action": "resume"}'
```
Restarting the webhook also re-triggers reconciliation from the persisted state on disk.

## Zero-downtime upgrade

The chart-managed Deployment uses a `Recreate` strategy (the recovery-state PVC is `ReadWriteOnce`
and needs the old Pod gone before the new one can mount it); `failurePolicy: Ignore` keeps that
brief gap safe — evictions are allowed through rather than blocked while the Pod restarts.

```bash
helm upgrade cube deploy/kubernetes/chart --namespace cube-system \
  --set evictionWebhook.enabled=true \
  --set images.evictionWebhook.tag=v2.0.0

kubectl rollout status deployment/eviction-webhook -n cube-system
# Roll back if needed:
kubectl rollout undo deployment/eviction-webhook -n cube-system
```

## Backup and restore of recovery state

```bash
# Backup
kubectl exec -n cube-system <eviction-webhook-pod> -- \
  cat /var/lib/eviction-webhook/recovery-state.json > recovery-state-backup.json

# Restore
kubectl cp recovery-state-backup.json \
  cube-system/<eviction-webhook-pod>:/var/lib/eviction-webhook/recovery-state.json
```

## Common commands

```bash
# Scale replicas
kubectl scale deployment/eviction-webhook --replicas=3 -n cube-system

# Inspect recovery state / audit log
kubectl exec -it -n cube-system <eviction-webhook-pod> -- \
  cat /var/lib/eviction-webhook/recovery-state.json | jq .
kubectl exec -it -n cube-system <eviction-webhook-pod> -- \
  tail -10 /var/log/eviction-webhook/events.ndjson | jq .

# Force-clear recovery state (use with care — sandboxes recorded as paused will
# no longer be tracked for automatic resume)
kubectl exec -it -n cube-system <eviction-webhook-pod> -- \
  sh -c 'echo "{}" > /var/lib/eviction-webhook/recovery-state.json'
```
