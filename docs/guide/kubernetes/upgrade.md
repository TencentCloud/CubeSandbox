# Upgrade

The goal in one sentence: **the control plane can roll in an orderly fashion; compute-plane upgrades recreate the `cube-node` Big Pod and interrupt existing sandboxes on that node.**

---

::: warning Preview version warning
The compute plane uses native `apps/v1` DaemonSets: image / resource / template changes **delete and recreate** the Big Pod (PodIP / netns change), which breaks existing sandbox networking. Compute-plane upgrades **recreate the `cube-node` Big Pod** (native DaemonSet) and **will interrupt existing sandboxes on that node**. Before upgrading, call CubeMaster’s isolate API, isolate the node for at least 60 seconds, and destroy the sandboxes on that node. Only after sandboxes are destroyed is it safe to upgrade the node.

**To smooth upgrades, you may use a Kubernetes plugin you are familiar with to achieve “in-place upgrade”** — update container images without recreating the Pod. After deploying the current version, carefully evaluate changes and test before upgrading further.

**These issues will be addressed in later versions. You are welcome to try the K8s deployment path and report issues and suggestions via Issues.**
:::

## Why does Big Pod recreate interrupt sandboxes?

CubeSandbox’s network (cubevs) hooks attach to the Pod’s network interface, and sandbox tap devices live in the same netns as the Pod. Recreating the Pod destroys that netns and breaks sandbox networking. Compute upgrades are therefore **disruptive** and need a maintenance window plus node isolation.

## What are you upgrading, and which workload do you change?

The compute plane is split into four lines. For day-to-day upgrades, **only change the image tags of the matching components**—avoid casually changing Big Pod env / volumeMount / container lists (those also recreate the Pod).

| What you want to upgrade | Which workload | What to change in values | Will it recreate the Big Pod? |
| --- | --- | --- | --- |
| cubelet / network-agent / wait-node-prep / slot images or resources | **Big Pod** (`cube-node`) | `images.cubelet`, etc. | **Yes** (interrupts sandboxes) |
| shim / kernel / guest artifacts | **Installer** | `images.cubeShim`, etc. | No (Big Pod template unchanged) |
| node-init / node preflight logic | **Bootstrap** | `images.nodeInit` | No (Big Pod template unchanged) |
| PVM host kernel-swap scripts | **cube-node-pvm** | `images.pvmHostBootstrap` | No (but the node may reboot) |

```text
Upgrade runtime components  →  only change Big Pod related images.*.tag (recreates Big Pod)
Upgrade toolbox artifacts   →  only change Installer related images.*.tag
Upgrade node preflight      →  only change images.nodeInit.tag
Upgrade PVM kernel swap     →  only change images.pvmHostBootstrap.tag
```

---

## Day-to-day upgrade (recommended path)

1. In your local `runtime-values.yaml` (the same values file used at first install), update the image **tags** you want to bump, for example:

```yaml
images:
  cubelet:
    tag: v0.5.2
  # Add others only if you need them together, e.g.:
  # networkAgent:
  #   tag: v0.5.2
  # cubeShim:
  #   tag: v0.5.2
```

Only change the keys you truly need to bump; leave other images alone. Full key names are in the [appendix](#appendix-image-key-cheat-sheet) at the end.

2. Run the upgrade with the same `-f` combination used at install:

**⚠️ Warning:** In production, canary-upgrade node by node and component by component. A full-fleet upgrade is very dangerous!

::: warning Preview version warning
Before bumping Big Pod runtime images, isolate the node and clear sandboxes; the upgrade will recreate the Big Pod.
:::

```bash
helm upgrade cube ./deploy/kubernetes/chart -n cube-system \
  -f runtime-values.yaml
# For TKE / single-node, continue to layer the same values-tke.yaml / values-single-node.yaml used at first install
```

### How do you confirm the upgrade succeeded?

```bash
# Big Pod is recreated: UID / PodIP will change; check new Pod Ready and node re-registration
kubectl get pods -n cube-system -l app.kubernetes.io/component=cube-node -o wide
kubectl get daemonset -n cube-system cube-node

# Control-plane Deployments roll as usual
kubectl get deploy -n cube-system
kubectl rollout status deploy/cube-master -n cube-system
```

Expect:

- Control-plane Pods finished rolling per Deployment strategy (`cube-master` uses Recreate; other CP Deployments use RollingUpdate)
- Compute: matching DaemonSet Pods run the new images and are Ready
- If you bumped Big Pod runtime: existing sandboxes on that node were interrupted; new sandboxes can be created; the node has re-registered with CubeMaster

---

## Red lines: these operations also recreate the Big Pod

Any of the following **recreates** the Big Pod → PodIP / netns change → existing sandboxes interrupt. Do them only in a planned maintenance window.

| Do not do casually | Why |
| --- | --- |
| Add/remove Big Pod containers (including changing slot count) | Changes the Pod template; the DaemonSet recreates the Pod |
| Change volumeMount / securityContext / container name / env directly | Same |
| Change `wait-node-prep` env / mount (bumping image also recreates) | wait is an initContainer; any template change recreates |
| Manually delete the Big Pod | Equivalent to rebuilding the data plane |
| Stuff artifact installs into the Big Pod | Breaks the split of duties; artifacts must go through Installer |

Also: `cubeNode.env`, `cubeNode.podAnnotations`, network-related env, `global.timezone`, and `cubeEgress.enabled` also change the Pod template — **not silent day-to-day upgrade items**.

---

## Special cases

### A. Changing PVM kernel pattern / boot args (will reboot)

For day-to-day bumps of only the `images.pvmHostBootstrap` image when the fingerprint still matches, the `pvm-not-ready` gate is generally **not** re-applied.

If you **intentionally change** `bootArgs` / kernel pattern (expecting the fingerprint to change), apply an ops gate **before** `helm upgrade` (`value=maintenance`, different from the Hook’s automatic `true` — old holds do not clear maintenance by default):

```bash
# 1. Confirm CNI and kube-proxy tolerate this NoSchedule taint
kubectl taint node <pvm-node> \
  cube.tencent.com/pvm-not-ready=maintenance:NoSchedule --overwrite

# 2. Update bootArgs / kernel-related config in runtime-values.yaml, then upgrade
helm upgrade cube ./deploy/kubernetes/chart -n cube-system \
  -f runtime-values.yaml
```

For example in values:

```yaml
bootstrap:
  pvmHostKernel:
    bootArgs: "nopti pti=off <new-arg>"
```
After the node recovers, only a new PVM init clears maintenance when the live fingerprint matches. Any step failure must not reboot. Details: [Architecture · PVM](./architecture.md#pvmcube-node-pvm).

To disable PVM on a node: remove that node’s `allow-pvm-bootstrap` label. **Do not** expect setting only `cubeNode.pvmGuestKernel.enabled=false` to quietly switch a node already running PVM back to bm.

### B. Clean uninstall and reinstall (last resort)

```bash
helm uninstall cube -n cube-system
sudo ./deploy/kubernetes/chart/scripts/cleanup-node-host.sh
helm upgrade --install cube ./deploy/kubernetes/chart \
  -n cube-system -f runtime-values.yaml
```

This removes Chart-managed objects; hostPath / kernel changes need the script and platform runbooks separately.

---

## Appendix: image key cheat sheet

Use this when you need “which image key maps to which container”:

| values key | Workload | Container |
| --- | --- | --- |
| `images.cubelet` | Big Pod | `cubelet` |
| `images.networkAgent` | Big Pod | `network-agent` |
| `images.waitNodePrep` | Big Pod / Bootstrap | Big Pod `wait-node-prep` init; Bootstrap write-ready also uses it |
| `images.cubeShim` | Installer | `cube-shim-install` |
| `images.cubeKernel` | Installer | `cube-kernel-install` |
| `images.cubeGuest` | Installer | `cube-guest-install` |
| `images.nodeInit` | Bootstrap | `wait-pvm-host` / `cube-node-init` |
| `images.pvmHostBootstrap` | cube-node-pvm | `pvm-host-bootstrap` / hold reconcile |

After changing policy such as `bootArgs` / `prepGeneration`, if you worry about accidentally touching the Big Pod template, run:

```bash
sh deploy/kubernetes/chart/scripts/test-big-pod-inplace-guard.sh
```

This guard requires **zero diff** on the Big Pod Pod template for those policy changes (so unrelated config bumps do not trigger recreate).

---

## Next steps

- [Architecture](./architecture.md)
- [Helm Install](./install.md)
- [FAQ](./faq.md)
