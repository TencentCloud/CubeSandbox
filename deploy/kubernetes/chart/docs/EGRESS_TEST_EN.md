# Local-veth EgressProxy Test Report

English | [中文](./EGRESS_TEST.md)

## 1. Result

The local-veth design in [EGRESS_VETH_EN.md](./EGRESS_VETH_EN.md) was validated
on a Cilium test cluster on 2026-07-28. Functional, failover, Big Pod handoff,
latency, throughput, long-connection, cleanup, and Helm gates passed.

NetworkPolicy used during acceptance was temporary and user-defined. It is not
a chart resource, built-in policy, or product recommendation, and it was
deleted after the test.

## 2. Environment

| Item | Value |
| --- | --- |
| CNI | Cilium |
| Compute nodes | 3 |
| Configured cluster CIDR count | 1 |
| Helm release | `cube`, final acceptance revision 65 |
| Proxy image | `20260728-review04` |
| Configurer image | `20260728-review06` |
| Cubelet/network-agent image | `20260728-cgroupv2-01` |

Cluster IDs, kubeconfig paths, node addresses, and sandbox identifiers are test
environment details and are intentionally omitted from the English public
summary. The Chinese source report retains the original evidence captured
during the test.

## 3. Static and local checks

The following checks passed:

- Helm lint and chart guards;
- sticky primary/standby failover;
- policy-rule, route-table, and iptables ownership conflicts;
- CIDR canonicalization and deduplication;
- ownership-scoped host cleanup;
- real veth lifecycle in a privileged test network namespace;
- cgroup v1 `tasks` and cgroup v2 `cgroup.procs` process migration;
- host-interface ownership conflicts;
- shell syntax and `git diff --check`;
- absence of tunnel tools/dependencies in Proxy and Configurer images.

Rendered persistent resources were limited to the `cube-egress` namespace,
primary/standby Proxy DaemonSets, and Configurer DaemonSet. No EgressProxy
StatefulSet, Service, PDB, NetworkPolicy, or tunnel Secret was rendered.

## 4. Data path

Every compute node had primary and standby local veth interfaces, policy rule
priority 10900 matching the Router NAT IP and route mark, an active default in
table 100, and an `unreachable default` fallback.

A real sandbox reached the allowed Service. Mark, host-forwarding, and Proxy
SNAT counters increased together, and the destination observed the local
primary EgressProxy Pod IP. Access to a non-cluster CIDR did not increment the
mark counter, proving that the request bypassed EgressProxy.

## 5. User NetworkPolicy

| Case | Result |
| --- | --- |
| Sandbox → allowed Service | Passed |
| Sandbox → denied Service | Timed out as required by the user policy |
| Ordinary Pod → denied Service | Passed; not captured by the Proxy path |
| Source observed by target | EgressProxy Pod IP |

The temporary policy was deleted after testing. Chart install, upgrade,
rollback, disablement, and uninstall do not manage user NetworkPolicy.

## 6. Failover and fail-closed behavior

| Scenario | Result |
| --- | --- |
| Primary veth down | Active route moved to standby |
| Service access after standby takeover | Passed |
| Primary recovered | Healthy standby remained active; no switch-back |
| Both veth paths down | Active default removed; `unreachable default` retained |
| Cluster access during dual failure | Timed out without main-route fallback |
| Both paths recovered | Active route and access recovered automatically |

Stopping only the primary Proxy health process, while leaving the veth UP,
also triggered standby takeover within 10 seconds. This proves that selection
does not rely on link state alone.

## 7. Big Pod replacement

All three `cube-node` Pods were rolled through a real Pod-template annotation
change. On the node hosting the sampled sandbox, the sandbox ID and creation
time, shim PID, network-agent PID, primary/standby veth ifindex, and table-100
active path remained unchanged. The sandbox continued to access a Kubernetes
Service after the rollout.

## 8. Performance

The same sandbox and 64 MiB payload were used for direct and Proxy paths.
Temporary bypass rules used for the direct baseline were removed immediately,
reconciliation was restored, and production mark/table-100 state was verified.

### 8.1 New-connection latency

Each target/path ran 200 new TCP+HTTP requests.

| Target | Additional P50 | Additional P99 | Errors | Gate |
| --- | ---: | ---: | ---: | --- |
| Same-node PodIP | 0.902 ms | 0.780 ms | 0/200 | Passed |
| Same-node Service IP | -0.651 ms | -1.723 ms | 0/200 | Passed |
| Cross-node PodIP | -0.102 ms | -0.636 ms | 0/200 | Passed |
| Cross-node Service IP | 0.305 ms | 0.412 ms | 0/200 | Passed |

Gates: additional P50 ≤ 1 ms, additional P99 ≤ 5 ms, error rate < 0.01%.
Negative deltas are sampling variation, not an acceleration claim.

### 8.2 Established-path latency

For 200 ICMP requests to the same cross-node PodIP, direct RTT P99 was
0.799 ms and EgressProxy RTT P99 was 0.845 ms. The estimated one-way additional
latency was approximately 0.023 ms, below the 2 ms gate.

### 8.3 Throughput

Single-connection runs transferred 64 MiB. Four-connection runs transferred
256 MiB total. Ratios use median duration from interleaved direct/Proxy runs.

| Target | Concurrency | Direct | Proxy | Ratio | Gate | Result |
| --- | ---: | ---: | ---: | ---: | ---: | --- |
| Same-node PodIP | 1 | 11.453 Gbit/s | 10.437 Gbit/s | 91.13% | ≥90% | Passed |
| Same-node PodIP | 4 | 11.108 Gbit/s | 9.740 Gbit/s | 87.69% | ≥85% | Passed |
| Same-node Service IP | 1 | 10.296 Gbit/s | 10.752 Gbit/s | 104.44% | ≥90% | Passed |
| Same-node Service IP | 4 | 10.800 Gbit/s | 9.394 Gbit/s | 86.99% | ≥85% | Passed |
| Cross-node PodIP | 1 | 2.476 Gbit/s | 3.174 Gbit/s | 128.23% | ≥90% | Passed |
| Cross-node PodIP | 4 | 1.490 Gbit/s | 1.472 Gbit/s | 98.80% | ≥85% | Passed |
| Cross-node Service IP | 1 | 2.703 Gbit/s | 2.748 Gbit/s | 101.67% | ≥90% | Passed |
| Cross-node Service IP | 4 | 1.460 Gbit/s | 1.426 Gbit/s | 97.68% | ≥85% | Passed |

Effective throughput is payload bytes × 8 ÷ elapsed time and excludes protocol
headers. Ratios above 100% are benchmark variance; acceptance requires only
that the Proxy path stays above the lower-bound gate.

## 9. Long connection

A single HTTP/1.1 keep-alive connection ran for 618.017 seconds with 605
successful requests and zero failures. A separate probe contaminated by the
temporary direct-baseline route modification was excluded and independently
retested without route changes.

## 10. Cluster acceptance

- Disabling EgressProxy removed the component-owned priority-10900 rule,
  table-100 routes, and `CUBE-EGRESS*` rules from all three nodes.
- Re-enabling recreated the expected data path on all nodes.
- All primary, standby, and Configurer Pods became Ready.
- All eight Helm test suites succeeded.
- No old StatefulSet, Service, PDB, NetworkPolicy, tunnel Secret, or tunnel
  interface remained.
- Release values exposed only `enabled` and `clusterCIDRs` for EgressProxy.

## 11. Test resources

Generic service/client resources are in
[egress-test-resources.yaml](../scripts/egress-test-resources.yaml), and the
long-connection probe is
[egress-long-connection.sh](../scripts/egress-long-connection.sh).

No user NetworkPolicy is stored in those files. The test sandbox, workloads,
and test namespace were deleted after acceptance.
