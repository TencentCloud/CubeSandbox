# eviction-webhook

Kubernetes ValidatingWebhook for intercepting Pod eviction requests to CubeSandbox resources, enabling graceful pause/resume recovery instead of permanent destruction.

## Problem Statement

When Kubernetes nodes experience resource pressure (memory/disk/PID), kubelet automatically initiates Pod eviction. For CubeSandbox sandbox Pods, eviction means the underlying MicroVM is immediately destroyed—losing all user state with no recovery path.

This component intercepts eviction requests and transforms them into:

1. **Node Isolation** — Prevents new sandbox scheduling to the pressured node
2. **MicroVM Pause** — Freezes sandbox (CPU halted, memory preserved)
3. **Automatic Resume** — Returns sandbox to running state when pressure subsides

## Quick Deployment

```bash
# 1. Generate TLS certificates
kubectl apply -f deploy/kubernetes/cert.yaml

# 2. Create authentication credentials
kubectl create secret generic eviction-webhook-auth \
  --from-literal=user_id=webhook-user \
  --from-literal=secret_key=webhook-secret \
  -n cube-system

# 3. Deploy all components
kubectl apply -f deploy/kubernetes/rbac.yaml
kubectl apply -f deploy/kubernetes/deployment.yaml
kubectl apply -f deploy/kubernetes/webhook.yaml

# 4. Verify
kubectl logs -f -n cube-system -l app=eviction-webhook
```

## Testing

```bash
go test -v ./...
go test -cover ./...
```

## Monitoring

Metrics exposed on port 8888:

```bash
kubectl port-forward -n cube-system svc/eviction-webhook 8888:8888
curl http://localhost:8888/metrics
```

## License

Apache-2.0
