# Upgrade

The goal in one sentence: **during upgrades, control-plane nodes can roll in an orderly canary fashion, while the compute node `cube-node` Big Pod is not recreated**.

---

::: warning Preview version warning
The in-place, zero-downtime upgrade flow for compute nodes is still being refined. Evaluate and test carefully before operating, to avoid recreating the `cube-node` Big Pod and losing existing sandboxes.
Before upgrading a node, call CubeMaster’s isolate API, isolate the node for at least 60 seconds, then proceed with the upgrade.
:::

## Why must the compute Big Pod not be recreated?

CubeSandbox’s network (cubevs) hooks attach to the Pod’s network interface, and the sandbox tap devices live in the same netns as the Pod’s network namespace. Recreating the Pod destroys that netns and breaks sandbox networking. To keep upgrades from affecting existing sandboxes, upgrade operations must not recreate the cube-node Pod.

## What are you upgrading, and which workload do you change?

The compute plane is split into four lines. For day-to-day upgrades, **only change the image tags of the matching components**—do not casually change Big Pod env / volumeMount / container lists.

| What you want to upgrade | Which workload | What to change in values | Will it recreate the Big Pod? |
| --- | --- | --- | --- |
| cubelet / network-agent / wait-node-prep / slot images or resources | **Big Pod** (`cube-node`) | `images.cubelet`, etc. | **No** (InPlace) |
| shim / kernel / guest artifacts | **Installer** | `images.cubeShim`, etc. | No |
| node-init / node preflight logic | **Bootstrap** | `images.nodeInit` | No (Big Pod should stay unchanged) |
| PVM host kernel-swap scripts | **cube-node-pvm** | `images.pvmHostBootstrap` | No (but the node may reboot) |

```text
Upgrade runtime components  →  only change Big Pod related images.*.tag
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
The in-place, zero-downtime upgrade flow for compute nodes is still being refined. Evaluate and test carefully before operating, to avoid recreating the `cube-node` Big Pod and losing existing sandboxes.
Before upgrading a node, call CubeMaster’s isolate API, isolate the node for at least 60 seconds, then proceed with the upgrade. After the upgrade, manually clear isolation.
:::

```bash
helm upgrade cube ./deploy/kubernetes/chart -n cube-system \
  -f runtime-values.yaml
# For TKE / single-node, continue to layer the same values-tke.yaml / values-single-node.yaml used at first install
```

### How do you confirm the upgrade succeeded InPlace?

```bash
# Big Pod UID / PodIP should match pre-upgrade
kubectl get pods -n cube-system -l app.kubernetes.io/component=cube-node -o wide

# Events should show a successful in-place update (exact name may vary slightly by version)
kubectl get events -n cube-system --field-selector reason=SuccessfulUpdatePodInPlace
```

Expect:

- Big Pod was **not deleted and recreated** (UID / IP unchanged)
- Existing sandboxes remain reachable
- Matching component versions are the new tags

---

## Red lines: these operations destroy “in-place upgrade”

Any of the following may **recreate** the Big Pod → PodIP / netns change → existing sandboxes interrupt. Do them only in a planned maintenance window.

| Do not do casually | Why |
| --- | --- |
| Add/remove Big Pod containers (including changing slot count) | Container set is the freeze surface |
| Change volumeMount / securityContext / container name / env directly | Same — will recreate |
| Change `wait-node-prep` env / mount (bumping image only is OK) | wait env/mount is frozen |
| Change `rollingUpdateType` to Standard, or manually delete the Big Pod | Equivalent to rebuilding the data plane |
| Stuff artifact installs into the Big Pod | Breaks the split of duties; artifacts must go through Installer |

Also: `cubeNode.env`, `cubeNode.podAnnotations`, network-related env, `global.timezone`, and `cubeEgress.enabled` also change the frozen template — **not day-to-day upgrade items**.

The InPlace whitelist is roughly only:

- Container **image**
- Slot **resources** (requires cluster `InPlacePodVerticalScaling`; see [install prerequisites](./install.md#1-prerequisites))
- Chart-managed `cube.tencent.com/slot-*` **annotations**

See also: [OpenKruise “In-Place Update” docs](https://openkruise.io/docs/core-concepts/inplace-update)

---

## Special cases

### A. Changing PVM kernel pattern / boot args (will reboot)

For day-to-day bumps of only the `images.pvmHostBootstrap` image when the fingerprint still matches, the `pvm-not-ready` gate is generally **not** re-applied.

If you **intentionally change** `bootArgs` / kernel pattern (expecting the fingerprint to change), apply an ops gate **before** `helm upgrade` (`value=maintenance`, different from the Hook’s automatic `true` — old holds do not clear maintenance by default):

```bash
# 1. Confirm CNI, kube-proxy, and kruise-daemon tolerate this NoSchedule taint
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
| `images.waitNodePrep` | Big Pod / Bootstrap | `wait-node-prep`; Bootstrap write-ready also uses it |
| `images.cubeShim` | Installer | `cube-shim-install` |
| `images.cubeKernel` | Installer | `cube-kernel-install` |
| `images.cubeGuest` | Installer | `cube-guest-install` |
| `images.nodeInit` | Bootstrap | `wait-pvm-host` / `cube-node-init` |
| `images.pvmHostBootstrap` | cube-node-pvm | `pvm-host-bootstrap` / hold reconcile |

After changing policy such as `bootArgs` / `prepGeneration`, if you worry about accidentally touching the Big Pod template, run:

```bash
sh deploy/kubernetes/chart/scripts/test-big-pod-inplace-guard.sh
```

This guard requires **zero diff** on the Big Pod Pod template for those policy changes.

---

## Next steps

- [Architecture](./architecture.md)
- [Helm Install](./install.md)
- [FAQ](./faq.md)
