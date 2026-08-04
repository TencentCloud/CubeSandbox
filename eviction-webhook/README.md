# eviction-webhook

A Kubernetes `ValidatingWebhook` that intercepts sandbox Pod eviction requests, preventing MicroVM
destruction and enabling transparent pause/resume recovery when node memory pressure clears.

---

## Background

CubeSandbox runs user workloads as MicroVMs inside Kubernetes Pods. When a node experiences memory
pressure, kubelet automatically evicts Pods to reclaim resources. **For sandbox Pods, eviction means
the underlying MicroVM is immediately and permanently destroyed — all user state is lost with no
recovery path.**

This is a critical reliability gap: users lose their running environment without warning.

---

## Problem

```
Node memory pressure
        │
        ▼
kubelet selects sandbox Pod for eviction
        │
        ▼
Pod evicted → MicroVM destroyed → user state lost ✗
```

There was no mechanism to:
- Intercept the eviction before it executes
- Preserve the MicroVM's in-memory state
- Restore the user's session when pressure clears

---

## Requirements

Develop an independent `eviction-webhook` component that:

1. **Intercepts** kubelet-initiated eviction requests for sandbox Pods at the Kubernetes API layer
2. **Denies** the eviction so the MicroVM is not destroyed
3. **Cordons** the pressured node to stop new sandbox scheduling
4. **Pauses** the MicroVM via CubeMaster (CPU halted, memory state preserved)
5. **Monitors** the node's `MemoryPressure` condition
6. **Resumes** automatically when pressure clears — user processes continue from the exact suspension point

**Out of scope:** This component does not modify CubeMaster, Cubelet, cube-lifecycle-manager, or
CubeProxy. All pause/resume operations are delegated to CubeMaster's existing APIs.

---

## Solution

### Architecture

```
kubelet eviction request (pods/eviction CREATE)
        │
        ▼
ValidatingWebhook ── objectSelector: cube.master.instance.type (sandbox Pods only)
        │
        ├── kubelet eviction  →  denied (allowed: false)
        │                        │
        │                        ▼
        │                   RecoveryManager
        │                        ├── CubeMaster.IsolateNode()    cordon node
        │                        ├── CubeMaster.PauseSandbox()   freeze MicroVM
        │                        └── persist state to disk
        │
        └── admin eviction (drain)  →  allowed through (maintenance unblocked)

NodeWatcher monitors Node.MemoryPressure condition
        │
        └── MemoryPressure: True → False
                │
                ▼
           RecoveryManager
                ├── CubeMaster.ResumeSandbox()    unfreeze MicroVM
                └── CubeMaster.UnisolateNode()    uncordon node
                        │
                        ▼
              User resumes from exact suspension point ✓
```

### Key Design Decisions

| Decision | Rationale |
|----------|-----------|
| `failurePolicy: Ignore` | Webhook outage must not block cluster-wide eviction |
| Admin evictions pass through | `kubectl drain` and maintenance workflows must not hang |
| All CubeMaster calls are async goroutines | 5 s webhook timeout; CubeMaster calls can take longer |
| Recovery state persisted to disk | Webhook restarts must not leave sandboxes permanently paused |
| Startup reconciliation (`ReconcileRestored`) | On restart, converge disk state with live cluster pressure |
| `objectSelector` on Pod label | Scopes the webhook to sandbox Pods only; zero impact on other workloads |

### Component Overview

| Package | Responsibility |
|---------|---------------|
| `internal/admission` | HTTP handler for `ValidatingWebhook`; eviction allow/deny decision |
| `internal/recovery` | State machine: cordons/uncordons nodes, pauses/resumes MicroVMs |
| `internal/nodewatch` | Watches `Node.MemoryPressure` condition transitions |
| `internal/cubemaster` | HTTP client for CubeMaster isolation and sandbox update APIs |
| `internal/podinformer` | Local Pod cache for label lookup on the hot webhook path |
| `internal/store` | NDJSON audit log writer |
| `internal/reporter` | Optional async event reporter to CubeMaster `/event/eviction` |
| `internal/telemetry` | Zap structured logging and OTel tracing initialisation |
| `internal/metrics` | Prometheus metrics definitions (8 metrics) |

---

## Test Results

### Unit and Integration Tests

All tests pass:

```
ok  internal/admission     coverage: 98.6%   (11 unit tests)
ok  internal/auth          coverage: 86.7%   (9 unit tests)
ok  internal/cubemaster    coverage: 92.9%   (26 unit tests)
ok  internal/recovery      coverage: 92.7%   (30 unit tests)
ok  internal/reporter      coverage: 92.0%   (11 unit tests)
ok  internal/store         coverage: 93.3%   (7 unit tests)
ok  internal/telemetry     coverage: 100.0%  (6 unit tests)
ok  test/integration       5 BDD scenarios
ok  test/e2e               TLS + full HTTP flow
```

All business-logic paths (eviction decision, recovery state machine, CubeMaster API calls) are fully
covered. The gap to 100% is exclusively defensive error-handling code that cannot be triggered in a
normal test environment:

| Reason | Affected packages | Example |
|--------|-------------------|---------|
| OS-level fault injection required | `auth`, `cubemaster`, `reporter` | `crypto/rand.Read` failure; `hmac.Hash.Write` failure — both guaranteed by the Linux kernel to never fail in practice |
| Standard-library marshal guarantee | `store`, `reporter`, `cubemaster` | `json.Marshal` on a struct with only string fields never returns an error |
| 5-minute async sleep boundary | `recovery` | `scheduleAPIEvictionRelief` sleeps `apiEvictionReliefDelay` before invoking the pressure checker; the error-return branch inside the goroutine is unreachable without waiting the full delay or modifying internal constants |
| Rare syscall failure | `recovery`, `cubemaster` | `os.Rename` on the same filesystem; `io.ReadAll` mid-connection disconnect — both require kernel-level or network-level fault injection |

Integration test scenarios (BDD — Given / When / Then):

| Scenario | Result |
|----------|--------|
| kubelet eviction → webhook | `allowed: false`, recovery triggered | ✅ |
| Admin / drain eviction → webhook | `allowed: true`, maintenance unblocked | ✅ |
| Eviction event → audit log | NDJSON line persisted with correct `EventID` | ✅ |
| Eviction event → RecoveryManager | `OnEviction` called with correct `NodeName` | ✅ |
| Multiple evictions on same handler | Each denied independently with matching UID | ✅ |

### Real Cluster E2E Test (2026-07-29)

End-to-end validation on a live TKE cluster with **real kubelet MemoryPressure** (not mocked):

| Step | Evidence | Result |
|------|----------|--------|
| Deploy `stress-ng` Pod (3600 M) to a compute node | Pod Running | ✅ |
| kubelet enters real MemoryPressure | `Eviction manager: attempting to reclaim memory` | ✅ |
| nodewatch detects pressure | `[nodewatch] MemoryPressure detected` | ✅ |
| Node cordoned | `[cubemaster] node isolated` | ✅ |
| Sandbox listed and paused | `listed sandboxes count=1` → `pause sandboxID=<id>` | ✅ |
| New sandbox scheduling blocked during pressure | `SCHEDULING_DISABLED=true` | ✅ |
| Pressure clears (kubelet `MemoryPressure=False`) | Node condition updated | ✅ |
| Sandbox resumed | `[cubemaster] sandbox resume` | ✅ |
| Node uncordoned | `[cubemaster] node unisolated` | ✅ |
| All nodes restored to `Ready=True / MemoryPressure=False` | Final cluster state | ✅ |

Latency: pressure detected → sandbox paused **< 1 s**; pressure cleared → sandbox resumed **< 1 s**.

---

## Deployment

### Prerequisites

- Kubernetes 1.22+
- cert-manager (for automatic TLS certificate injection)
- CubeMaster running and accessible

### Deploy

```bash
# 1. TLS certificate (cert-manager injects caBundle automatically)
kubectl apply -f deploy/kubernetes/cert.yaml

# 2. CubeMaster authentication credentials
kubectl create secret generic eviction-webhook-auth \
  --from-literal=user_id=<user_id> \
  --from-literal=secret_key=<secret_key> \
  -n cube-system

# 3. RBAC, Deployment, and ValidatingWebhookConfiguration
kubectl apply -f deploy/kubernetes/rbac.yaml
kubectl apply -f deploy/kubernetes/deployment.yaml
kubectl apply -f deploy/kubernetes/webhook.yaml

# 4. Verify
kubectl rollout status deployment/eviction-webhook -n cube-system
kubectl logs -f -n cube-system -l app=eviction-webhook
```

### Configuration

| Environment Variable | Description | Default |
|---------------------|-------------|---------|
| `CUBE_MASTER_URL` | CubeMaster service endpoint | *(required)* |
| `CUBE_AUTH_ENABLE` | Enable HMAC authentication | `true` |
| `EVENT_REPORT_ENABLE` | Report eviction events to CubeMaster | `false` |
| `AUDIT_LOG_PATH` | Local NDJSON audit log path | `/var/log/eviction-webhook/events.ndjson` |
| `RECOVERY_STATE_PATH` | Persisted recovery state path | `/var/lib/eviction-webhook/recovery-state.json` |
| `METRICS_ADDR` | Prometheus metrics listen address | `:8888` |
| `DEBUG` | Enable debug-level logging | `false` |

---

## Monitoring

### Prometheus Metrics (`:8888/metrics`)

| Metric | Type | Description |
|--------|------|-------------|
| `eviction_webhook_intercepted_total` | Counter | Evictions intercepted, by node / instance\_type / reason |
| `eviction_webhook_requests_total` | Counter | Total webhook requests, by operation / allowed |
| `eviction_webhook_request_latency_seconds` | Histogram | Webhook request latency |
| `eviction_webhook_recovery_duration_seconds` | Histogram | Sandbox recovery duration |
| `eviction_webhook_cubemaster_api_latency_seconds` | Histogram | CubeMaster API call latency |
| `eviction_webhook_cubemaster_errors_total` | Counter | CubeMaster API errors |
| `eviction_webhook_isolated_nodes_total` | Counter | Total nodes isolated |
| `eviction_webhook_paused_sandboxes` | Gauge | Current number of paused sandboxes |

```bash
kubectl port-forward -n cube-system svc/eviction-webhook 8888:8888
curl http://localhost:8888/metrics
```

---

## Running Tests

```bash
# Unit tests with coverage
go test -cover ./...

# Integration tests only
go test -v ./test/integration/...

# Run all tests with a timeout
go test -timeout 60s ./...
```

---

## Further Reading

- [Design Document](docs/eviction-webhook-research.md) — detailed background, K8s eviction mechanics, CubeMaster API research
- [Operations Guide](docs/OPERATIONS.md) — troubleshooting, zero-downtime upgrades, alert rules
- [Quick Reference](docs/QUICK_REFERENCE.md) — command cheatsheet, key metrics table

---

## License

Apache-2.0
