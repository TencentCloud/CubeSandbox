# Kubernetes post-install validation

Run nine checks through the existing SDK E2E runner: deployment preflight,
shared create/kill/command/file cases, Service ClusterIP and FQDN access,
public DNS/HTTPS, and CubeProxy routing to a declared custom port.

## Requirements and invocation

- An installed Helm release with chart-managed CubeOps and Ready compute Pods
  on Linux nodes with KVM. The runner needs kubectl, Helm and SDK test dependencies.
- A Ready template with Python 3, trusted TLS roots, cluster DNS configured,
  and port 8088 declared in `exposedPorts` (or the custom port below).
- Reachable CubeAPI and CubeProxy; use the main README for authentication settings.
- Read access to Helm metadata, nodes, workloads, PVCs and Services;
  `services/proxy` access to CubeOps; permission to create/read/delete test
  namespaces and create/read their Pods and Services. Optional `pods/exec`
  permission enables read-only KVM/runtime/socket checks. SSH is not required.

From `tests/e2e/sdk_compat`:

```bash
export SDK_E2E_K8S_CONTEXT=my-cluster
export SDK_E2E_K8S_NAMESPACE=cube
export SDK_E2E_K8S_RELEASE=cube
export CUBE_API_URL=http://127.0.0.1:3000
export CUBE_TEMPLATE_ID=tpl-your-ready-template
# Set CUBE_API_KEY and reachable CubeProxy settings as described in README.
export SDK_E2E_REPORT_DIR=reports/kubernetes
pytest --run-e2e --k8s-post-install
```

Both flags are required. Preflight checks Helm, workloads, PVCs, image pulls,
registered compute placement and reported capacity before the existing SDK/template
preflight. Reports include versions, node kernels, images, DNS IPs and discoverable
CNI DaemonSets. Unavailable optional exec checks produce warnings; observed missing
KVM/runtime/socket assets fail. Actual creation proves scheduling capacity.

## Network paths and cleanup

Each Service case creates a unique namespace, unprivileged HTTP Pod and Service.
The sandbox is pinned to a healthy registered compute node; the HTTP Pod prefers
a different node. Reports record `same-node` or `cross-node`. Single-node results
do not cover cross-node routing.

Creation disables general internet access and explicitly allows CoreDNS and the
Service IP. FQDN access uses the guest's configured cluster DNS without modifying
`/etc/resolv.conf`. HTTP responses must match this run's marker. The CubeProxy case
starts a guest HTTP server on the template's declared port and verifies its marker.
Service tests require the native CubeSandbox `distribution_scope` extension;
other SDK backends skip them. General allow/deny tests remain in the existing suite.

| Optional variable | Default |
| --- | --- |
| `SDK_E2E_K8S_CLUSTER_DOMAIN` | `cluster.local` |
| `SDK_E2E_K8S_MOCK_IMAGE` | `busybox:1.37`; must provide sh/httpd |
| `SDK_E2E_K8S_CUSTOM_PORT` | `8088`; must be declared by the template |
| `SDK_E2E_K8S_PUBLIC_URL` | `https://example.com/`; must return HTTP 200 |

Existing trace, JSONL, retries and debug retention settings apply. Namespace cleanup
checks UID ownership; sandbox cleanup confirms API absence. Cleanup failures fail
this profile. Forced termination can leave resources; use the
`kubernetes_mock_created` report event to locate them. Start with serial execution.
Report skipped checks and retained sandboxes alongside results; skipping internet
checks does not validate public egress. These tests do not certify a CNI configuration.
