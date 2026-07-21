# FAQ

Common issues when deploying and running the Helm Chart, grouped by topic. Check this section first; if still stuck, see [Helm Install](./install.md), [Architecture](./architecture.md), and [Upgrade](./upgrade.md). When filing an Issue, include the [question template](#question-template) at the end.

::: tip Resource names in commands
Examples below assume Release name `cube` and namespace `cube-system` (resource names like `cube-master`, `cube-secret`). If your Release name differs, substitute `<release>-…`, or use `app.kubernetes.io/component=…` label selectors (independent of Release name).
:::

## Contents

- [Install and validation](#install-and-validation)
- [Nodes and scheduling](#nodes-and-scheduling)
- [Control plane / database](#control-plane--database)
- [Compute / PVM / sandbox](#compute--pvm--sandbox)
- [CubeProxy / TLS / DNS](#cubeproxy--tls--dns)
- [Egress networking](#egress-networking)
- [Upgrade, rollback, and uninstall](#upgrade-rollback-and-uninstall)
- [Image builds](#image-builds)

---

## Install and validation

### `helm install` errors saying you must configure `placement.*.nodeSelector`

The Chart uses `templates/validate.yaml` to forbid “wildcard” deploys that could harm nodes. Add an explicit nodeSelector, for example:

```yaml
placement:
  controlPlane:
    nodeSelector:
      cube.tencent.com/cube-control: "true"
```

Related validations:

| Error keyword | Meaning |
| --- | --- |
| `cube-node requires placement.compute.nodeSelector` | Compute nodes must be specified explicitly |
| `…placement.pvm… allow-pvm-bootstrap` | The PVM DaemonSet selector must include this label; do not put it under `placement.compute` |
| `placement.compute… must not include …allow-pvm-bootstrap` | Putting it under compute makes every compute node pull the large PVM image |
| `cubeProxy.enabled=true requires placement.controlPlane.nodeSelector` | Proxy runs on control-plane nodes |
| `configureClusterDNS=true requires cubeProxy.domain` | Injecting cluster DNS requires a sandbox domain |

How to label nodes: [Helm Install · Label nodes](./install.md#3-label-nodes-and-role-taints).

### Compute DaemonSet Ready count is short

First check the four native DaemonSets:

```bash
kubectl -n cube-system get daemonset \
  -l 'app.kubernetes.io/component in (cube-node,cube-node-installer,cube-node-bootstrap,cube-node-pvm)'
```

| Symptom | Action |
| --- | --- |
| `DESIRED=0` | No nodes match `placement.compute` → add `cube-node=true` label |
| `CURRENT < DESIRED` | Blocked by taints → check `compute` taint and tolerations |
| `READY < CURRENT` | Pod is up but not Ready → for Big Pod, check `wait-node-prep` and bootstrap first; see [Compute](#compute--pvm--sandbox) |

### CubeAPI `/health` fails in `helm test`

Usually CubeAPI is not Ready yet, or MySQL / migration has not finished:

```bash
kubectl -n cube-system logs -l app.kubernetes.io/component=api --tail=200
kubectl -n cube-system logs -l app.kubernetes.io/component=master -c cube-master --tail=200
```

- CubeMaster embeds schema migration; first run may take a few minutes; use `--timeout 90m` at install
- If logs show MySQL connection refused → see [Control plane / database](#control-plane--database)

### `helm test` hangs with neither timeout nor result

Only Helm **3.13+** makes `--timeout` apply to test hooks. Add `--logs`:

```bash
helm test cube -n cube-system --logs --timeout 20m
```

Inspect failed test Pods individually:

```bash
kubectl -n cube-system get pods -l app.kubernetes.io/component=test
kubectl -n cube-system logs <test-pod-name>
```

---

## Nodes and scheduling

### Can I apply fewer labels?

Not recommended. The Chart uses explicit label authorization because:

- `pvm-host-bootstrap` swaps the host kernel and may reboot
- The compute plane is privileged / hostPath; mis-scheduling is costly

In production, manage labels uniformly with GitOps / Terraform.

### Can a control-plane node also be a compute node?

Technically yes (single-node trial); not recommended in production — resource contention and upgrade strategies differ.

When co-locating:

1. Apply two independent labels on the same node: `cube-control=true` and `cube-node=true` (do not overwrite with a single key)
2. Layer `values-single-node.yaml` so both control and compute tolerate both role taints
3. Add `allow-pvm-bootstrap=true` when PVM is needed

When scaling pure compute nodes, only apply `cube-node` + the `compute` taint — **do not** apply `cube-control`. Steps: [Helm Install · Single-node](./install.md#42-single-node-trial-one-machine-is-both-control-and-compute).

### PVC / PV stays Pending

```bash
kubectl get sc
kubectl get pvc -n cube-system
kubectl describe pvc -n cube-system <pvc-name>
kubectl -n cube-system describe pod <pod-name>
```

**Generic clusters (no SC created by default):**

- No default StorageClass → set `persistence.storageClassName`, or switch to hostPath (see [Install · Prepare values](./install.md#5-prepare-the-values-file))
- Specified SC / CSI does not exist → install the provisioner first
- SC is `WaitForFirstConsumer`, but the Pod cannot schedule due to selector / taint → make the Pod schedulable first

**TKE + `values-tke.yaml`:** CBS disks are created after the Pod is placed; common causes are insufficient node resources, or nodeSelector vs CBS availability zone mismatch.

### Temporarily taking one compute node out for maintenance

Destroy sandboxes on the node first, then:

```bash
kubectl drain <node> --ignore-daemonsets --delete-emptydir-data=false
# After maintenance
kubectl uncordon <node>
```

`cube-node` will come back and re-register once the node is schedulable again.

---

## Control plane / database

### cube-master cannot connect to MySQL

Check in order (commands below assume Release `cube`, namespace `cube-system`; for other Release names, replace the resource name prefix with `<release>`):

1. Is built-in MySQL Running: `kubectl -n cube-system get pods -l app.kubernetes.io/component=mysql`
2. Was the Secret password changed by mistake: `kubectl -n cube-system get secret cube-secret`
3. Connectivity from inside the container:

```bash
kubectl -n cube-system exec cube-mysql-0 -- mysql -uroot \
  -p"$(kubectl -n cube-system get secret cube-secret -o jsonpath='{.data.mysql-root-password}' | base64 -d)" \
  -e 'show databases'
```

External MySQL (probe from the **CubeAPI** Pod; env vars are `CUBE_SANDBOX_MYSQL_*`, not `CUBE_MYSQL_HOST`):

```bash
kubectl -n cube-system exec -l app.kubernetes.io/component=api -- \
  sh -c 'nc -zv "$CUBE_SANDBOX_MYSQL_HOST" "$CUBE_SANDBOX_MYSQL_PORT"'
```

On the CubeMaster side, the MySQL address is in the mounted `conf.yaml` (Chart-rendered), not injected via the env vars above.

### Schema not fully migrated after upgrade / migration stuck

CubeMaster runs embedded goose migration at startup. Check logs:

```bash
kubectl -n cube-system logs -l app.kubernetes.io/component=master -c cube-master | grep -i migrat
```

You can also connect to the DB and inspect `goose_db_version`. If a previous abnormal exit left a stuck lock, handle it manually per DB state (operate carefully).

### External MySQL 8 `caching_sha2_password` auth failure

The Chart no longer forces `mysql_native_password`. CubeMaster / CubeAPI drivers support `caching_sha2_password`. If the user still uses the old plugin:

```sql
ALTER USER 'cube_user'@'%' IDENTIFIED WITH caching_sha2_password BY '<new-password>';
FLUSH PRIVILEGES;
```

### How do I use cubemastercli?

```bash
# Interactive shell (bashrc in the image auto-fills --address / --port)
kubectl -n cube-system exec -it -l app.kubernetes.io/component=cubemastercli -- bash
cubemastercli node list
cubemastercli sandbox list
```

Or one-liner (same as Chart `NOTES.txt`):

```bash
kubectl -n cube-system exec deploy/cube-cubemastercli -- \
  sh -lc 'cubemastercli --address "$CUBEMASTERCLI_ADDRESS" --port "$CUBEMASTERCLI_PORT" node list'
```

---

## Compute / PVM / sandbox

### `pvm-host-bootstrap` keeps restarting / node reboots

Runs on **`cube-node-pvm`** (only nodes with `allow-pvm-bootstrap`). Most “repeated restarts” are actually normal reboots after kernel swap. Check logs first:

```bash
kubectl -n cube-system logs -l app.kubernetes.io/component=cube-node-pvm -c pvm-host-bootstrap --tail=100
kubectl -n cube-system logs -l app.kubernetes.io/component=cube-node-bootstrap -c wait-pvm-host --tail=50
kubectl -n cube-system logs -l app.kubernetes.io/component=cube-node -c wait-node-prep --tail=50
```

Normal path (summary): ensure `pvm-not-ready` → drain dependent Pods → invalidate ready markers → kernel swap / reboot → write `pvm-host-ready` and clear the gate → bootstrap writes `node-prep-ready` → Big Pod `wait-node-prep` init exits → run containers start. Full details: [Architecture · PVM](./architecture.md#pvmcube-node-pvm).

If stuck on wait-node-prep: first confirm bootstrap is Ready and the `/var/lib/cube-node-bootstrap/node-prep-ready` fingerprint. If the node never reboots after kernel swap, it usually lacks reboot permission or GRUB was not updated — log into the node, `reboot` manually, and check:

```bash
uname -r     # Should contain cubesandbox.pvm.host (matches Chart default DESIRED_KERNEL_PATTERN)
cat /proc/cmdline | grep -o pti=off
```

### `cube-node-init` reports `/dev/kvm` unavailable

```bash
kubectl debug node/<node> -it --image=busybox -- sh
ls -la /dev/kvm
lsmod | grep kvm
```

Cloud VMs need PVM installed; physical machines need VT-x / AMD-V enabled.

### cube-node is Ready, but no sandboxes / node not registered

```bash
kubectl -n cube-system exec -l app.kubernetes.io/component=cubemastercli -- \
  sh -lc 'cubemastercli --address "$CUBEMASTERCLI_ADDRESS" --port "$CUBEMASTERCLI_PORT" node list'
```

- `healthy: true` → you can create sandboxes
- Not in the list → check registration logs:

```bash
kubectl -n cube-system logs -l app.kubernetes.io/component=cube-node -c cubelet --tail=200 | grep -i register
```

A common cause is inability to reach CubeMaster (network / DNS).

### Sandbox start is slow (>10s) while the node is mostly idle

- **First** use of a template: rootfs is pulled from the control plane, may take 5–30s; subsequent starts of the same template are usually within ~1s
- Check whether `/data/cubelet` is full or IOPS-bound: `iostat -x 1`
- Many Paused sandboxes occupying disk without cleanup

### Rough limits on sandboxes per node?

1. **Memory**: roughly hundreds of MB to several GB per sandbox
2. **Disk**: CoW rootfs; depends on `/data/cubelet` capacity (default loopback is only **25G**; see [Install · Compute node data disk](./install.md#compute-node-data-disk-datacubelet))
3. **KVM count**: typically at most hundreds per machine, subject to kernel parameters

`Pause` can drive inactive sandbox CPU/RSS near 0 (disk is not released). Operationally: Pause after N minutes idle, Destroy after longer.

### `/data/cubelet` out of space / want to change data disk size

Default `bootstrap.nodeInit.dataCubelet.loopback.size` is `25G`; on first run bootstrap creates `/data/cubelet-xfs.img` and mounts it at `/data/cubelet`.

- **Not yet installed / image file does not exist**: change `size` in values (e.g. `200G`) and reinstall or re-run bootstrap.
- **Image already created**: changing values again **does not** auto-expand. In production, prefer disabling loopback and using a pre-mounted large XFS disk; if you must rebuild loopback, delete the old img in a maintenance window (this clears data at that path).

Config examples: [Helm Install · Compute node data disk](./install.md#compute-node-data-disk-datacubelet).

### How do I disable PVM on a node?

**Remove that node’s label** (do not only flip the Chart default switch):

```bash
kubectl label node <node> cube.tencent.com/allow-pvm-bootstrap-
```

`cubeNode.pvmGuestKernel.enabled=false` mainly affects the **first-install default**; nodes already running PVM try to stay as-is across upgrades.

To skip PVM kernel swap cluster-wide: do not apply `allow-pvm-bootstrap`, and you may set `bootstrap.pvmHostKernel.enabled=false`.

Self-check the current guest:

```bash
readlink /usr/local/services/cubetoolbox/cube-kernel-scf/vmlinux
# or
readlink /var/lib/cube-node-bootstrap/vmlinux-active
```

---

## CubeProxy / TLS / DNS

### Sandbox domain does not resolve

1. **In-cluster** (including guests that follow node DNS): `nslookup test.cube.app`
   - No answer → check whether CoreDNS contains `# BEGIN cube-sandbox-dns` (followed by the Release name), or Job logs: `kubectl -n cube-system logs job/cube-cluster-dns-apply`
   - Answer should be the CubeProxy ClusterIP: `kubectl -n cube-system get svc cube-proxy -o wide`
2. **Guest DNS**: default `cubeNode.dns.sandbox.followNodeDns=true`; you can set `cubeNode.dns.sandbox.nameservers` explicitly
3. **Outside the cluster**: point `cube.app` / `*.cube.app` at Ingress / LB

### Ingress / external entry does not work

CubeProxy **no longer** occupies node 80/443. Check:

- Whether IngressClass / Ingress exists
- Whether passthrough annotations match your Controller (defaults assume nginx-ingress)
- TKE: `values-tke.yaml` disables Ingress by default and uses LoadBalancer (CLB); point DNS at EXTERNAL-IP
- No Ingress: set `cubeProxy.ingress.enabled=false` and wire to the Service yourself

### selfSigned certificate browser warnings

Expected for trials. For production use:

```yaml
cubeProxy:
  tls:
    mode: existingSecret   # or certManager
    existingSecret: my-cube-tls
```

### Changing `cubeProxy.advertiseIP` seems to have no effect

In the Chart, `cubeProxy.advertiseIP` is only a **human-readable hint field** (`values.yaml` comments call it an optional hint). It is **not** written into Proxy / DNS / Service templates; changing it and restarting Proxy does not change cluster behavior.

What you actually need to change:

- The address that external DNS / LB points to
- Or `cubeProxy.service` / Ingress-related settings

If you only want to roll Proxy replicas:

```bash
kubectl -n cube-system rollout restart deploy/cube-proxy
```

---

## Egress networking

### Sandbox cannot reach the internet

Egress denies by default and only allows the whitelist:

```bash
kubectl -n cube-system logs <cube-node-pod> -c cube-egress --tail=200 | grep -i deny
```

Adjust `cubeEgress` values as needed, or update the whitelist via the CubeMaster API.

### TLS errors after outbound MITM

Trust the egress CA distributed by the Chart:

```bash
kubectl -n cube-system get secret cube-egress-ca \
  -o jsonpath='{.data.cube-root-ca\.crt}' | base64 -d > cube-ca.crt
# Inject into the template / guest trust store, e.g. /etc/ssl/certs/
```

(Secret name defaults to `<release>-egress-ca`; certificate key defaults to `cube-root-ca.crt`, not `ca.crt`.)

Or use `cubeEgress.ca.mode: existingSecret` to reuse an enterprise CA.

### `cube-egress-net` CrashLoopBackOff

It depends on `cube-dev` created by the main containers:

```bash
kubectl -n cube-system logs <cube-node-pod> -c cube-egress-net --tail=100
```

- Log shows `interface cube-dev not present` → **network-agent / cubelet** have not created `cube-dev` yet; usually self-heals; if longer than ~5 minutes, check those two containers’ logs
- Repeated `rule reapply failed` → iptables too old (needs nft) or conflict with CNI rules

---

## Upgrade, rollback, and uninstall

### Will `helm upgrade` interrupt existing sandboxes? Will Pod IP change?

**Bumping Big Pod runtime images / changing the Pod template: yes.** `cube-node` is a native DaemonSet; changes recreate the Pod (UID / IP / netns change) and interrupt existing sandboxes on that node. Bumping only Installer / Bootstrap / PVM while leaving the Big Pod template untouched can leave the Big Pod unchanged. Steps and red lines: [Upgrade](./upgrade.md).

Typical Big Pod recreate triggers: bump `images.cubelet` and other runtime images, add/remove containers, change volumeMount / securityContext / container name / env.

### Does `helm rollback` roll back the host kernel?

**No.** Helm only manages K8s objects. Kernel / GRUB / fstab / XFS etc. need a separate host rollback runbook.

### Data remains on nodes after `helm uninstall`

By design. Uninstall only deletes Chart-managed objects. Common leftovers on compute nodes:

```bash
sudo rm -rf /data/cubelet /data/cube-shim /data/snapshot_pack \
  /data/log/Cubelet /data/log/CubeShim /data/log/CubeVmm \
  /usr/local/services/cubetoolbox /var/lib/cube-node-bootstrap /tmp/cube \
  /data/cubelet-xfs.img
```

PVM kernels also need package removal, GRUB changes, and reboot. You can also use the Chart’s `deploy/kubernetes/chart/scripts/cleanup-node-host.sh`.

Whether PVC/PV are deleted depends on the StorageClass `reclaimPolicy` (TKE’s `cube-cbs-wffc` is commonly `Delete`; `Retain` leaves them behind).

---

## Image builds

### `build-cube-images.sh` download hangs

- By default it pulls the all-in-one package from **GitHub Releases**; in China set `MIRROR=cn` to use CNB (not SourceForge)
- Or pre-download into `${BUILD_ROOT}/downloads/` (default `BUILD_ROOT=/tmp/cube-kubernetes-images-<version>`); the script skips download when local files are present

See [`deploy/kubernetes/images/README.md`](https://github.com/TencentCloud/CubeSandbox/blob/master/deploy/kubernetes/images/README.md).

### Building arm64

```bash
ONE_CLICK_ARCH=arm64 \
PUSH=1 REGISTRY=<your-registry> IMAGE_TAG=v0.5.1-arm64 \
./deploy/kubernetes/images/build-cube-images.sh
```

Needs an arm64 machine or buildx multi-arch.

### Rebuild only one image

Yes. Pass image names to the script (unnecessary downloads / `SOURCE_REF` exports are skipped as needed):

```bash
./deploy/kubernetes/images/build-cube-images.sh cubelet
./deploy/kubernetes/images/build-cube-images.sh cubelet cube-shim
```

Full list: `./deploy/kubernetes/images/build-cube-images.sh --help`; details in [`images/README.md`](https://github.com/TencentCloud/CubeSandbox/blob/master/deploy/kubernetes/images/README.md).

If the release package is already unpacked, point `PACKAGE_DIR_OVERRIDE` at that directory to avoid re-downloading. For local development you can also use `LOCAL_BIN=1` / `--local` to layer `_output/bin` into the image.

### Old curl reports unknown `--retry-all-errors`

The current script **does not** add `--retry-all-errors` by default; it only enables it when `CURL_RETRY_ALL_ERRORS=1` is set explicitly and the local curl supports that flag. Usually you can ignore this; if an old fork hard-coded the flag, remove it or upgrade curl.

---

## Question template

```text
Chart version:
K8s version: (kubectl version)
Environment: (TKE / self-managed / k3s / single-node …)
Relevant values snippet: (redact passwords)
Failing component: Pod name + kubectl describe + kubectl logs
kubectl -n cube-system get pods -o wide
```

---

## Next steps

- [Helm Install](./install.md)
- [Architecture](./architecture.md)
- [Upgrade](./upgrade.md)
