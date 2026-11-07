# EgressProxy Upgrade and Rollback Guide

English | [中文](./UPGRADE.md)

This guide covers only the optional node-local veth EgressProxy
(`egressProxy.enabled`). It does not describe Big Pod replacement: that behaviour
is provided by the base chart and by the official
[Upgrade](https://cubesandbox.com/guide/kubernetes/upgrade) guide, and this PR
does not change it.

`egressProxy` has no effect on `cubeNode.hostNetwork`, the cubelet PID, sandbox
cgroups, hostPath layout, or shim reconnection. Enabling or disabling it only
adds or removes the EgressProxy data plane.

## 1. What the feature installs

With `egressProxy.enabled=true` the chart adds:

| Resource | Network | Purpose |
| --- | --- | --- |
| `<release>-egress-configurer` DaemonSet | host network | Reconciles host iptables, ip rule and table 10901; selects the healthy proxy |
| `<release>-egress-proxy-primary` DaemonSet | Pod network | Owns `cube-egress-p0`, forwards and SNATs to its own Pod IP |
| `<release>-egress-proxy-standby` DaemonSet | Pod network | Owns `cube-egress-p1`, same forwarding path |
| `cube-egress` namespace | — | Namespace for the three DaemonSets, kept on uninstall |

All three DaemonSets use `OnDelete`. The exposed values are
`egressProxy.enabled`, `egressProxy.clusterCIDRs` and `egressProxy.routeTable`
(default 10901); priority 10900, the mark, veth names and the link-local prefix
are fixed.

`routeTable` is configurable because no single number is universally free:
table 100 belongs to CubeEgress (`local default dev lo` for L7 TPROXY), 101-110
are commonly taken by Cilium/kube-router, 200-252 by Calico/systemd-networkd,
and 253-255 are reserved by the kernel. The default 10901 sits in the
application-private high range next to the fixed rule priority 10900. Override
it when a node already uses that table.

`egressProxy.enabled=true` requires `cubeNode.network.cubeRouter.enabled=true`;
the chart fails validation otherwise. Cube Router adds env to the Big Pod
template, so the first enablement **replaces the Big Pod once**. The EgressProxy
resources themselves are independent of that replacement, but plan the rollout
accordingly.

The chart accepts 1 to 32 entries in `clusterCIDRs`, IPv4 only.

## 2. Upgrade boundary and limits

Only sandbox traffic whose destination matches `clusterCIDRs` is marked and
steered into table 10901. Everything else keeps the node main route.

Real limits, all visible to users:

- policy selection is per EgressProxy Pod / node, not per sandbox;
- primary and standby do not share conntrack, so failover breaks established
  connections;
- IPv4 only; there is no dual-stack support;
- `clusterCIDRs` is manual and must list every Pod and Service CIDR;
- the chart never creates, modifies, deletes, or recommends a NetworkPolicy;
- the Configurer runs as host network with `NET_ADMIN`, and the Proxy
  initContainer is privileged, so a scoped Pod Security admission exemption is
  required;
- there is no automated drain, rollout orchestrator, or metric endpoint;
- CubeEgress coexistence is preserved rather than traded away; see section 3.

## 3. Coexistence with CubeEgress

CubeEgress L7 interception and EgressProxy run side by side on the same node.
The `cube-egress-net` sidecar is always deployed with the Big Pod and is never
removed by enabling EgressProxy.

Three separations keep them independent:

| Dimension | CubeEgress | EgressProxy |
| --- | --- | --- |
| Ingress | `-i cube-dev` | `-i cube-router -s <router-nat-ip>` |
| iptables chains | `TRANSPROXY` (mangle) | `CUBE-EGRESS-MARK` / `CUBE-EGRESS` / `CUBE-EGRESS-SNAT` |
| fwmark | `0xCE010000` / `0xCE020000`, mask `0xFFFF0000` | `0x1000`, mask `0x1000` |
| Routing table | 100 (`local default dev lo`) | 10901 (default) |

Steering is decided by CubeVS `classify_egress_flow()`, in this order:

1. a flow matching an L7 rule gets the L7 mark and is injected into `cube-dev`,
   handled by CubeEgress and **never entering** EgressProxy;
2. other flows to `clusterCIDRs` get `0x1000` and enter `cube-router`, handled by
   EgressProxy;
3. everything else keeps the node main route.

L7 policy therefore wins over NetworkPolicy: a Service captured by an L7 rule is
still allowed by that rule even if a NetworkPolicy would deny it. Do not attach
L7 rules to Services that must be denied by NetworkPolicy.

Confirm coexistence is live:

```bash
# L7 chain counters should grow with L7 traffic
iptables -t mangle -L TRANSPROXY -v -n
# both routing tables should be present
ip rule show | grep -E 'lookup (100|10901)'
```

## 4. Preflight

```bash
helm lint deploy/kubernetes/chart
bash deploy/kubernetes/chart/scripts/test-egress-proxy-guard.sh
bash deploy/kubernetes/chart/scripts/test-egress-policy-rules.sh
bash deploy/kubernetes/chart/scripts/test-egress-cleanup.sh
```

Render the production values and confirm:

- the EgressProxy image tag or digest is what you intend to ship;
- exactly the Configurer, primary, standby and namespace render;
- the render contains no NetworkPolicy, tunnel Secret, Service, StatefulSet or
  PodDisruptionBudget for EgressProxy;
- `clusterCIDRs` covers every Pod and Service CIDR on the target cluster and
  does not overlap the sandbox CIDR.

On each target node confirm before rollout:

- both Proxy Pods and the Configurer are Ready;
- the active peer answers its health check;
- the mangle chain marks `clusterCIDRs` before Service DNAT;
- the policy rule matches the cube-router input interface, the Router NAT IP and
  the route mark;
- table 10901 holds both the active default route and the fail-closed
  `unreachable default`.

## 5. Recommended rollout

EgressProxy images are not rolled by `helm upgrade` alone because all three
DaemonSets are `OnDelete`. Process one node at a time:

1. replace and validate the Configurer, and confirm host rules and the active
   route stayed healthy;
2. replace the inactive Proxy;
3. confirm the new Proxy is Ready and can take traffic;
4. switch the active path to the updated Proxy;
5. replace the former active Proxy;
6. keep at least one healthy Proxy on the node throughout, and do not force a
   switch-back after failover.

```bash
kubectl -n cube-egress get daemonset
kubectl -n cube-egress delete pod <old-egress-configurer-pod>
kubectl -n cube-egress delete pod <old-inactive-egress-proxy-pod>
```

Do not delete both Proxies on the same node at the same time: with both paths
down, table 10901 keeps only the `unreachable default` and marked cluster traffic
fails closed by design.

## 6. Acceptance

For each updated node verify:

- Configurer, primary Proxy and standby Proxy are Ready;
- the pre-DNAT mark still matches only `clusterCIDRs`;
- the active table-100 default points at the expected veth;
- a user NetworkPolicy allow case succeeds and a deny case is blocked;
- ordinary Pod traffic, host-network Pod traffic and node traffic are not
  captured by the EgressProxy rules;
- with both Proxies stopped, marked cluster traffic times out instead of
  falling back to the main route;
- existing veth ifindex and the table-100 active path are unchanged after a
  Big Pod replacement.

Readiness alone does not prove policy enforcement. Always run a business-level
sample through an allowed and a denied destination.

## 7. Disable and uninstall

Setting `egressProxy.enabled=false` removes the three DaemonSets. The
Configurer `preStop` runs ownership-checked cleanup for its chains, the fixed
FORWARD rule, the priority-10900 policy rule and the table-10901 routes.

```bash
helm upgrade <release> deploy/kubernetes/chart -n <namespace> \
  -f runtime-values.yaml --wait
```

Verify after the Configurer Pod is gone:

```bash
ip rule show | grep 10900
ip route show table 10901
iptables-save | grep CUBE-EGRESS
```

There is no `post-delete` cleanup hook. Uninstall relies on each Configurer
Pod's `preStop` within `terminationGracePeriodSeconds: 60`. Uninstall **must**
use `--wait`, and the three checks above must still be run per node afterwards.

A forced Pod deletion, an unreachable node, or a bypassed kubelet skips
`preStop`. In that case run the same cleanup image on the node after it
recovers; cleanup is not automatic in that scenario.

## 8. Rollback

```bash
helm history <release> -n <namespace>
helm rollback <release> <revision> -n <namespace> --wait --timeout 10m
```

Helm rolls back only Kubernetes resources. It does not restore host veth
interfaces, iptables state, policy rules or table-10901 routes. Those are only
cleaned when the new values set `egressProxy.enabled=false` or the release is
uninstalled.

`rp_filter` needs no rollback: it is set only on the `cube-egress-p0/p1`
interfaces, which disappear with the Proxy Pod, so the node-wide baseline is
never touched.

Rolling back the Proxy image follows the same manual order as section 5: after
rollback, continue using whichever path is currently healthy and do not force a
switch-back.

## 9. Release gate

Promote only after:

1. Helm lint, template and the guard scripts pass.
2. One node completes enable, Proxy image update, disable and re-enable.
3. NetworkPolicy allow and deny results match the user's policy.
4. Cleanup leaves no `CUBE-EGRESS*` chain, priority-10900 rule or table-100
   route behind.
5. The Pod Security admission exemption is granted and documented.
6. Images, values, Helm revisions and the tested CNI version are recorded.
