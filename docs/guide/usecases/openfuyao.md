---
title: "openFuyao: Running CubeSandbox on Kubernetes"
author: Wang Wentao, Li Aihua
date: 2026-09-17
tags:
  - kubernetes
  - deployment
  - ebpf
  - production
lang: en-US
---

# openFuyao: Running CubeSandbox on Kubernetes

## Business Context

openFuyao is an open-source community for general-purpose and intelligent computing cluster software innovation, with compute infrastructure built entirely on K8s. The openFuyao Agent Sandbox SIG team packaged CubeSandbox's control plane, data plane, and compute plane as standard K8s workloads, achieving "K8s manages the nodes, CubeSandbox manages the sandboxes." The proposal was submitted to the community as [discussion #636](https://github.com/TencentCloud/CubeSandbox/discussions/636). The official native K8s deployment in v0.6.0 is similar to their self-built model, allowing a smooth switch.

## Key Challenges

- **Two operational systems**: CubeSandbox's early physical-machine form (systemd + Docker Compose) coexisting with the K8s platform meant separate channels for releases, monitoring, and certificate rotation — doubled mental cost.
- **Same-node network failure**: after the compute plane moved into Big Pods, same-node access to sandbox mapped ports always failed (issue [#443](https://github.com/TencentCloud/CubeSandbox/issues/443)).
- **cubelet process model vs. PID 1**: cubelet assumed it ran under host systemd; its "process B kills its parent and becomes an orphan" model crashed containers inside pods.
- **Feature semantics drift**: host-mount's "host" drifted from the node to the Big Pod; CubeVS's default-deny CIDR list covers nearly all K8s pod/cluster ranges.
- **Two-scheduler conflict**: K8s schedules pods while CubeMaster schedules sandboxes; tight pod resources.limits backfire Cube's overcommit via cgroups.

## Solution with Cube Sandbox

- **Big Pod deployment architecture**: cubelet + network-agent + cube-egress + CubeShim + CubeHypervisor packaged as a privileged DaemonSet (hostPath /dev/kvm, /data/cubelet; sandboxes run inside the Big Pod); CubeAPI/CubeMaster/CubeProxy as Deployment+Service (CubeMaster mounts a PV for templates); MySQL/Redis as StatefulSet; cluster CoreDNS reused with cube.app forwarding.
- **Network diagnosis chain**: 5 controlled scenarios pinning "same-node inbound traffic to mapped ports" as the failing case → nettrace/TAP packet capture → bpftool → bpf_printk logs; root cause was eBPF same-node forwarding missing pseudo-header fields, failing checksum validation. The issue drove upstream diagnosis, fixed in v0.4.0 (PR [#469](https://github.com/TencentCloud/CubeSandbox/pull/469) references issue #443).
- **Orphan-process solution (zero cubelet code changes)**: tini as PID 1 + bash entrypoint starting cubelet in the background + pidfile + preStop hook, fully preserving cubelet's original process model.
- **Four semantic-shift lessons**: CubeMaster on a PV + local templates on persistent hostPath (following the node, not the pod); compute-plane pods need hostPath volumes + privileged mode for real node directory passthrough; explicitly allow_out in-cluster service CIDRs as needed; network-agent fixed via "resolve gateway IP from routing table → exact-match by IP in neighbor table → lenient filtering accepting NUD_STALE/NUD_PROBE" for pods where the gateway MAC is unavailable.
- **Scheduling division**: K8s scheduling kept at node level, sandbox bin-packing left to CubeMaster overcommit; compute-plane pod CPU/memory limits given generously or left unset.

## Results and Benefits

- Sandbox compute nodes are elastically scaled and rolling-upgraded by K8s like ordinary workloads, without intruding on APIServer/Scheduler/etcd — standard primitives only.
- Deep usage of template distribution, snapshot, pause-resume, host-mount, egress, network policies, E2B interface, and ARM adaptation.
- issue #443 was referenced by the upstream v0.4.0 fix PR — a model case of practice feeding back upstream.
- Five capability requests to the community: cross-machine pause/resume and cross-machine Snapshot startup (supported in v0.7.0 preview), control/data-plane isolation with zero-downtime upgrades, higher creation throughput, and compatibility with eBPF CNIs like cilium/calico.

## References

- Full case study: [Running CubeSandbox on Kubernetes: Deployment and Production Practice](/blog/posts/2026-09-17-openfuyao)
- Cube Sandbox source: [TencentCloud/CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
