# 最近模板提交与 S3 场景复现手册

> 整理日期：2026-10-29。目标分支：`fix/artifact-download-s3-direct`。
> 用途：先共同复现、保留证据，再逐项修复；不是“全部问题已经解决”的验收报告。
> 本文命令尚未在真实 Linux / S3 / KVM 环境执行。`PASS`、`FAIL`、`SKIP`、`未触发`必须分开记录。
> 当前只整理中文手册，英文版本尚未同步。

## 1. 本次看哪些提交和场景

| 场景 | 对应提交 / issue | 要验证什么 | 建议顺序 |
|---|---|---|---|
| D1 | `f80e1c2c` | Cubelet 直连 S3/MinIO 下载 ext4，Master 不搬运字节 | 第一批 |
| D2 | `f80e1c2c` | TC 本地盘 / CBS / CFS 的 ext4 由真正持有文件的 TC 直传 | 第一批 |
| D3 | `f80e1c2c` | 短 TTL、重新取票、续传、授权失败、完整性检查 | 第一批 |
| B1 | `74207466` | 模板 CoW backend 默认 xfs，显式 s3，非法值被拒绝 | 第一批 |
| P1 | `f80e1c2c` | 模板列表真实 DB 分页，包括 job-only 模板 | 第一批 |
| R1 | `f80e1c2c` | rootfs 已存在 / 半成品后 AppSnapshot 能安全重试 | 第二批 |
| R2 | #2063 + 当前未提交补丁 | full redo 换 artifact 后，definition、replica、冷热缓存一致 | 第二批 |
| R3 | #2078 | 失败构建后的同规格新构建是否卡在 BUILT / CLEANUP_PENDING | 第二批 |
| N1 | `f80e1c2c` / #1965 | 节点真正退役后可删除副本，不能维持虚假 READY | 第二批 |
| L1 | `95510d23` / #1925 | pause 保留连接、restore reset 后恢复尾延迟和执行正常 | 第一批 |
| L2 | #1678 / 已包含的 #1763 修复 | 删除源模板后，依赖它的 snapshot 仍能恢复 | 第二批 |
| L3 | `b3f71c98` / #1952 | 没有泛域名时，固定IP:port通过E2B header路由 | 第一批 |
| S1 | #2077 | 同一 lvstore exports 超过 256 后重新 attach | 最后，独占环境 |
| S2 | #1971 / #2091 | checkpoint/WAL 重放及计算节点重启后的数据一致性 | 最后，独占环境 |
| E1 | #2064 | `--env` 覆盖同名 key，但不能丢掉镜像其他 ENV | 第一批 |

最近三项提交的范围来自提交说明，不把它们的历史性能数字当作本机验收阈值。

### 必须分清两个“s3”

- **artifact 存储**：`CUBE_ARTIFACT_STORE_BACKEND=s3|fs`，决定构建出的 ext4 从哪里下载。D1/D2 测这个。
- **沙箱 CoW backend**：模板 `--backend xfs|s3`，决定 pause/snapshot 包的存储和恢复。B1/L1/S1/S2 测这个。
- `artifact=s3 + CoW=xfs` 是有效组合，适合先隔离验证直下载。
- `tpl merge` 迁移历史 artifact；**不把既有 xfs 沙箱改成 s3**。
- CBS/CFS 是 TC 的文件存储介质，不是新的 CoW backend 枚举。

## 2. 测试环境与记录

### 2.1 最小拓扑

1. Linux 控制面：CubeMaster、TC、CubeOps、CubeAPI；可以同机。
2. 两个专用计算节点 A/B，使用一致的版本、内核、CPU 兼容信息。
3. B 最好是新节点，未缓存本轮 artifact。不要为了“冷下载”删除正在使用的 rootfs、CoW 对象或 NVMe namespace。
4. 独立测试 bucket；artifact bucket 和 CubeS3lvol bucket/prefix 分别记录。
5. L1 的 s3 分支及 S1/S2：明确启用 CubeS3lvol，确认 package 的 `remote_status=ready`。仅配置 artifact S3 不够。
6. Docker / KVM、`jq`、Python SDK；流量取证可用 `tcpdump`。S1/S2 另需 root、`nvme-cli`、`fio`、AWS CLI。

**危险场景边界**：节点删除、断网、重启、SIGKILL、lvstore attach 只能在已隔离并清空其他业务的专用机器上执行。S3lvol dataplane 脚本使用固定 RPC socket、设备及状态文件，必须串行，不能和已有 CubeS3lvol 服务混跑。

### 2.2 版本必须能区分基线与补丁

- 提交基线：`f80e1c2c`，包含 S3 直下载等改动。
- #2063 修复目前在工作区，**只记录 HEAD 会漏掉补丁**。分别保存 `git diff` 和实际部署组件版本。
- 部署记录至少包含 Master、TC、Cubelet、CubeShim/VMM、CubeS3lvol、SDK 版本及镜像 digest。
- 对照环境使用同一个 OCI image digest、CPU/内存、节点及配置。不要在有改动的仓库直接 reset/checkout 来切对照版本。

在仓库根目录设置以下变量；替换所有 `<...>`，不要把密钥填进共享文档：

```bash
export MASTER_HOST='<控制面可达IP或域名>'
export MASTER_PORT=8089
export TC_BASE='http://<实际持有artifact的TC>:8090'
export CUBE_API_URL='http://<CubeAPI可达地址>:3000'
export CUBE_PROXY_NODE_IP='<客户端可达的CubeProxy数字IP>'
export CUBE_PROXY_PORT_HTTP='<实际HTTP数据面端口>'
export NODE_A='<cubeopscli查到的节点ID>'
export NODE_B='<新计算节点ID>'
export OCI_IMAGE='ghcr.io/tencentcloud/cubesandbox-base:latest'
export RUN_ID="$(date -u +%Y%m%d-%H%M%S)"
export EVIDENCE="$PWD/repro-evidence/$RUN_ID"
umask 077
mkdir -p "$EVIDENCE"
set -o pipefail
cm() { cubemastercli --address "$MASTER_HOST" --port "$MASTER_PORT" "$@"; }

git rev-parse HEAD > "$EVIDENCE/head.txt"
git diff --stat > "$EVIDENCE/diff-stat.txt"
git diff -- CubeMaster/pkg/templatecenter > "$EVIDENCE/master-patch.diff"
cubeopscli node list --json > "$EVIDENCE/nodes-before.json"
python3 -c 'import importlib.metadata; print(importlib.metadata.version("cubesandbox"))'
```

`OCI_IMAGE` 是仓库文档已有的示例；正式对照要替换为已验证可启动、健康检查可用的固定 digest。示例 probe 为 `49983 /health`，自定义应用请使用它真实提供的端口和路径。

若尚未安装SDK，先在测试用Python虚拟环境、仓库根目录执行`python3 -m pip install -e sdk/python`，再执行上面的版本记录；本文Python片段使用Cube SDK接口，不混用E2B对象的`connect()`签名。

SDK 配置以仓库 `examples/snapshot-rollback-clone/README.md` 和 `sdk/python` 为准；启用 API 鉴权时，通过受控环境注入`CUBE_API_KEY`（SDK发送`X-API-Key`），自签CA使用`SSL_CERT_FILE`。`CUBE_API_URL`只配置控制面；没有泛域名DNS时，上面`CUBE_PROXY_NODE_IP`和`CUBE_PROXY_PORT_HTTP`必须填写，SDK会连接实际IP并保留虚拟Host。已有可信泛域名/TLS路由时按原配置，不强行改用明文IP。不要关闭鉴权来消除测试报错。

### 2.3 直下载配置核对，不要混用历史方案草稿

| 配置 | 用途 |
|---|---|
| `CUBE_ARTIFACT_STORE_BACKEND=s3` | artifact 上传并存储于 S3/MinIO |
| `CUBE_ARTIFACT_STORE_BACKEND=fs` | artifact 存储于 TC 可读取的文件系统 |
| `CUBE_S3_ENDPOINT` / `CUBE_S3_NODE_ENDPOINT` | 控制面 endpoint / 计算节点可达 endpoint |
| `CUBE_S3_BUCKET` / `CUBE_S3_REGION` | artifact bucket / region |
| `CUBE_ARTIFACT_STORE_FS_ROOT` | TC 文件存储根目录 |
| `CUBE_TEMPLATE_CENTER_DOWNLOAD_ADDR` | 计算节点可达的 TC 下载地址；不能填只对控制面有意义的 loopback |
| `CUBE_ARTIFACT_DOWNLOAD_SIGNING_KEY` | Master 与 TC 的下载签发/验签配置；由部署生成并安全分发 |
| `common.artifact_download_ttl` | 票据有效期，默认 `1h`，合法范围 `1m` 至 `24h`；由 Master 配置文件提供，不使用历史 TTL 环境变量 |
| `CUBE_TEMPLATE_CALLBACK_TOKEN` | 构建/删除控制通道配置，与下载 key 不是同一个用途 |

修改服务配置后要确认它已进入实际进程/容器环境；仅在操作终端 `export` 不会改变已有 systemd 服务。不要打印完整 `.one-click.env` 或 Secret 来取证。

**当前接口**是 `GET /internal/template/artifacts/:artifact_id/download-ticket`（无请求body）和 TC `GET/HEAD /internal/artifacts/:artifact_id/ext4`。[修复报告](../../dev/template-reliability-fix-plan.md) 第 0 节记录当前实施与实际验证，第 1 节以后是原始设计，不能用其中的历史路径和状态代替当前代码。统一测试入口、验收矩阵与清理流程见 [整体测试指南](../../dev/template-reliability-test-guide.md)。

## 3. 通用操作和只读取证

### 3.1 创建基线模板

先在 artifact=S3、CoW=xfs 环境运行：

```bash
cm tpl create-from-image \
  --image "$OCI_IMAGE" --writable-layer-size 4Gi \
  --alias "repro-$RUN_ID-xfs" --backend xfs \
  --node "$NODE_A" --expose-port 49983 --probe 49983 --probe-path /health \
  --detach --json | tee "$EVIDENCE/create.json"
export JOB_ID="$(jq -er '.job.job_id' "$EVIDENCE/create.json")"
export CUBE_TEMPLATE_ID="$(jq -er '.job.template_id' "$EVIDENCE/create.json")"
cm tpl watch --job-id "$JOB_ID" --json | tee "$EVIDENCE/create-final.json"
cm tpl status --job-id "$JOB_ID" --json
```

watch 失败时保留返回码和 job 信息，不继续把失败模板当 READY 基线。若不同发布包返回结构有差异，先读取响应和 `--help`，不要生成假的 template/job ID。

### 3.2 从模板创建并做真实命令检查

```bash
python3 - <<'PY'
import os
from cubesandbox import Sandbox

scope = [os.environ["NODE_B"]]
sb = Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"], distribution_scope=scope)
print("sandbox_id=", sb.sandbox_id, flush=True)
r = sb.commands.run("printf 'repro-ok\\n'")
print("exit_code=", r.exit_code, "stdout=", repr(r.stdout), "stderr=", repr(r.stderr))
assert r.exit_code == 0 and "repro-ok" in r.stdout
sb.kill()
PY
```

先通过 `tpl redo --node B` 分发，再用 B 的 `distribution_scope` 创建。创建返回成功不等于可执行；必须检查 exit code 和实际 stdout。

### 3.3 DB 对照：MySQL 只读 SQL

通过现有受控 DB 客户端执行，`@tpl` 替换为本轮模板。PostgreSQL 用原生列查询及 JSON 操作符，不直接复制 MySQL 的 `JSON_EXTRACT`。

```sql
SET @tpl = '<本轮template-id>';
SELECT template_id, status, display_name, storage_backend, rootfs_artifact_id,
       JSON_UNQUOTE(JSON_EXTRACT(request_json,
         '$.containers[0].image.annotations."cube.master.rootfs.artifact.id"')) AS request_artifact_id,
       JSON_UNQUOTE(JSON_EXTRACT(request_json,
         '$.containers[0].image.annotations."cube.master.rootfs.artifact.sha256"')) AS request_sha256,
       JSON_UNQUOTE(JSON_EXTRACT(request_json,
         '$.containers[0].image.annotations."cube.master.rootfs.artifact.ticket_url"')) AS ticket_url,
       JSON_UNQUOTE(JSON_EXTRACT(request_json,
         '$.containers[0].image.annotations."cube.master.rootfs.artifact.download_origin"')) AS download_origin
FROM t_cube_template_definition WHERE template_id = @tpl AND deleted_at IS NULL;
SELECT job_id, operation, status, phase, artifact_id, error_message, updated_at
FROM t_cube_template_image_job WHERE template_id = @tpl ORDER BY id;
SELECT node_id, node_ip, status, phase, artifact_id, cleanup_required, error_message
FROM t_cube_template_replica WHERE template_id = @tpl AND deleted_at IS NULL;
SELECT a.artifact_id, a.status, a.generation, a.storage_backend, a.storage_ref,
       a.object_key, a.owner_tc_download_base, a.ext4_sha256, a.ext4_size_bytes
FROM t_cube_rootfs_artifact a
WHERE a.artifact_id IN (
  SELECT artifact_id FROM t_cube_template_image_job WHERE template_id = @tpl
) ORDER BY a.id;
```

不要直接把全量 `request_json` / `result_json` / presigned URL 发群里。新模板的持久 request 应有稳定 `ticket_url`、`download_origin`、ID、SHA256、size；不应存 runtime `refresh_token` 或旧下载 `token`。历史 artifact 行的 `artifact_url` 非空不自动判错，需区分兼容数据。

### 3.4 日志与网络证据

在对应 Linux 节点分别执行，时间窗口覆盖操作前后：

```bash
sudo journalctl --since '20 minutes ago' -o short-iso \
  -u cube-sandbox-cubemaster.service \
  -u cube-sandbox-cube-templatecenter.service \
  -u cube-sandbox-cubelet.service \
  -u cube-sandbox-s3lvol.service > "$EVIDENCE/services.log"
```

组件可能另写文件日志，补收部署中实际配置的 CubeShim/VMM、TC、s3lvol 日志。Kubernetes 使用实际 namespace/pod/container 名收集，不把 one-click unit 名当 pod 名。

需要证明字节路径时，在计算节点 B 设置实际数字 IP：

```bash
export MASTER_IP='<Master数字IP>'
export TC_IP='<TC数字IP>'
export OBJECT_IP='<本轮实际对象存储数字IP>'
sudo timeout 180 tcpdump -i any -nn -q \
  "host $MASTER_IP or host $TC_IP or host $OBJECT_IP" \
  > "$EVIDENCE/flows-B.log" 2> "$EVIDENCE/flows-B.err"
```

在另一终端发起分发。只记录包元信息；不要加 `-A`、`-X` 或保存包含认证信息的原始 pcap。`timeout` 到期退出 124 是取证窗口结束，不是业务失败。TC与Master同IP时仅IP不能区分，结合8090/8089端口、实际连接和各服务字节量判定。S3域名可能解析多个IP，漏抓IP不能判定“没有S3流量”。

## 4. 第一批：正常链路和最近提交

### D1：S3/MinIO 直下载，Master 不代理 ext4

1. artifact backend=s3，先用 CoW=xfs；确认 object endpoint 从 B 可达，TLS可信。
2. 按 3.1 创建模板，记下 artifact ID、object key、SHA256、size。
3. 在新节点 B 开始 3.4 的取证，在控制面执行：

```bash
cm tpl redo --template-id "$CUBE_TEMPLATE_ID" --node "$NODE_B" --json \
  | tee "$EVIDENCE/direct-s3-redo.json"
export REDO_JOB_ID="$(jq -er '.job.job_id' "$EVIDENCE/direct-s3-redo.json")"
cm tpl watch --job-id "$REDO_JOB_ID" --json \
  | tee "$EVIDENCE/direct-s3-redo-final.json"
```

4. 执行 3.2。记录 B 下载/校验完成、模板副本状态和命令输出。
5. **通过**：B先向Master取票，随后向对象存储发GET；ext4大流量不经过Master；落地size/SHA256匹配，沙箱可执行。
6. **失败**：仍走Master旧下载字节流、签名host被改写、403/签名错误、将TC认证header带到对象存储、校验失败却仍发布可用模板。

常见误测：B已有缓存；只看模板READY；将GET签名URL拿去HEAD；控制面endpoint可达但node endpoint不可达；S3上传失败后悄悄改成本地来源。

### D2：TC 文件直传；Master 不共享该目录也能工作

1. 使用另一套 artifact=fs 的测试配置，不把 D1 已发布的 S3 行直接改成 fs。
2. 让文件仅存在于持有它的TC；Master不挂这个目录。优先独立TC/独立卷，**不要删除Master上的共盘文件制造条件**。
3. 创建新模板并向冷节点B分发，重复3.2和流量取证。
4. **通过**：B向真正持有artifact的TC `:8090`直连，Master不执行本地stat/读流；TC-local、CBS、CFS均使用同一HTTP读路径。
5. **负例**：配置一个不持有文件的TC地址，必须明确失败，不能静默改回Master中转。CFS共享可读与本地盘无副本不能混为一谈。

TC重启/CBS重挂载后重复一次，记录owner locator、实际可读文件与下载地址。端口可连接不代表该TC持有本artifact。

### D3：短期票据、过期重取、续传、权限及完整性

**正常路径**：默认1h与2h分别运行D1/D2；在受控取票观测中检查 `server_time`、`expires_at`、artifact ID/generation、source、SHA256、size，`source`实际值为`s3`或`template_center`。响应URL/token是敏感bearer凭证，不共享原值。

当前没有现成CLI/普通API领取refresh capability；合法值只由Master向Cubelet的分发/AppSnapshot RPC注入，持久化request会删除它。正例通过真实create/redo自动取票，不从`tpl info`猜取、不填写假token或自行签发。Master GET取票的header为`X-Cube-Node-ID`与`X-Cube-Artifact-Refresh-Token`；TC下载使用`X-Cube-Node-ID`与`X-Cube-Artifact-Token`，S3请求不携带这些内部header。

**无凭证负例**，使用真实artifact/node ID，不需要取出内部密钥：

```bash
export ARTIFACT_ID='<本轮artifact-id>'
curl -sS -o "$EVIDENCE/ticket-denied.json" -w '%{http_code}\n' \
  "http://$MASTER_HOST:$MASTER_PORT/internal/template/artifacts/$ARTIFACT_ID/download-ticket" \
  -H "X-Cube-Node-ID: $NODE_B"
curl -sS -o /dev/null -w '%{http_code}\n' \
  "$TC_BASE/internal/artifacts/$ARTIFACT_ID/ext4" \
  -H "X-Cube-Node-ID: $NODE_B"
```

预期分别403和401。仅自报node ID不构成授权；Master还校验refresh capability、当前节点、replica phase及generation。取票只允许`DISTRIBUTING`、`DISTRIBUTED`、`SNAPSHOTTING`阶段，不包含`READY`；不要把“分发结束后重放旧runtime请求被拒绝”误判为过期bug。

TC下载handler的正负例为：有效GET 200、合法Range 206、非法/越界Range 416、HEAD无body；非法ID/node 400；无效/过期/错node票401；旧generation/非READY 410；artifact或文件缺失404。当前未实现handler级403/429或自设ETag，代理额外返回的状态需单列。HEAD/Range依赖`http.ServeContent`，现有TC测试并未单独覆盖它们，需真实补测。

**精确故障复现**优先现有 httptest 单测，不手工篡改生产时间、密钥、WAL或DB：

- Master：`TestArtifactDownloadTTL`、`TestBuildArtifactDownloadTicketForS3UsesFreshBoundedURL`、`TestBuildArtifactDownloadTicketDoesNotFallBackToStoredS3URL`。
- TC：`TestArtifactDownloadHandlerValidatesTokenIdentityAndGeneration`，当前篡改/错node=401，旧generation=410；`directArtifactPath`相关测试验证受控文件路径。
- Cubelet：`TestDownloadArtifactRefreshesTicketAndResumes`、`TestFetchArtifactTicketRejectsExpiredOrWrongIdentity`、`TestFetchArtifactTicketRejectsUnexpectedOrigin`、`TestDownloadArtifactStopsOnTicketUnauthorized`、`TestDownloadArtifactRejectsMismatchedContentRange`、`TestFetchS3TicketDoesNotForwardInternalHeaders`。

真实弱网补测需专用测试代理：先半包断连，再使旧票失效，观测重新取票和Range；不把完整200响应追加到半文件。故障注入必须只影响本轮下载，不影响控制面心跳。**大文件下载时间长不等于票据过期复现**，开始校验有效的流可能跨过expires_at继续完成；需要后续重连才能验证重取。

TTL设为30s或25h时签发应明确失败，不能变成无限有效；具体发生在构建/签发入口，以服务错误为准，不宣称进程启动阶段一定拒绝。

### B1：CoW backend API 与派生继承

1. CLI分别创建省略`--backend`、`--backend xfs`、`--backend s3`的模板；s3组确认CubeS3lvol已启用。
2. CubeAPI `POST /templates`的字段为`backend`，不是`storageBackend`。在API已有鉴权配置下，用现有SDK或HTTP客户端提交：

```json
{
  "image": "<已验证image digest>",
  "writableLayerSize": "4Gi",
  "backend": "s3",
  "nodes": ["<测试节点ID>"],
  "exposedPorts": [49983],
  "probePort": 49983,
  "probePath": "/health"
}
```

3. 正例：省略/空值默认xfs；`s3`及API的大小写规范化按服务实现；`backend=fs`应在API被拒绝，不提交构建job。
4. 从s3模板创建沙箱，再pause/snapshot/from-snapshot，核对backend持续为s3。已有xfs产物不能通过create参数或`tpl merge`换后端。
5. 不只核对返回字段；至少各执行一次文件写读和恢复。

### P1：列表分页与 job-only 资源

准备至少55条专用可见模板，其中一部分构建尚未产生definition；无需全成功，避免一次铺满内存。记录本轮列表范围，别批量删除其他模板。

```bash
cm tpl ls --json > "$EVIDENCE/list-default.json"
cm tpl ls --limit 10 --offset 0 --json > "$EVIDENCE/list-0.json"
cm tpl ls --limit 10 --offset 10 --json > "$EVIDENCE/list-10.json"
cm tpl ls --limit 200 --offset 0 --json > "$EVIDENCE/list-200.json"
cm tpl ls --all --json > "$EVIDENCE/list-all.json"
```

通过：默认有限页50、上限200；静态数据下翻页不漏/不重复，total与可见集合一致，job-only未被丢掉，删除/内部隐藏snapshot不污染集合。`--all`内部逐页读取；输出格式以CLI为准，不假定它等于单页JSON envelope。

同时观察数据库实际查询：LIMIT/OFFSET发生在可见集合查询内，副本/最新job只聚合本页，不是全表读后切片。仅API输出10行不能证明DB分页。并发创建/更新时Offset允许漂移，不据此声称快照一致遍历。

### L3：固定IP:port的E2B header路由（#1952）

使用一个已运行、允许测试客户端访问的sandbox，保留其ID。`PROXY_BASE`必须是客户端可达的CubeProxy固定地址，不是CubeAPI地址；无需泛域名DNS。

```bash
export PROXY_BASE='http://<CubeProxy固定IP>:<实际HTTP端口>'
export SANDBOX_ID='<本轮已运行sandbox-id>'
curl -sS -o /dev/null -w '%{http_code}\n' "$PROXY_BASE/health" \
  -H "E2b-Sandbox-Id: $SANDBOX_ID"
curl -sS -o /dev/null -w '%{http_code}\n' "$PROXY_BASE/health" \
  -H "E2b-Sandbox-Id: $SANDBOX_ID" -H 'E2b-Sandbox-Port: 49983'
curl -sS -o /dev/null -w '%{http_code}\n' "$PROXY_BASE/health"
curl -sS -o /dev/null -w '%{http_code}\n' "$PROXY_BASE/health" \
  -H 'E2b-Sandbox-Id: 00000000000000000000000000000000'
```

仓库当前E2E预期依次204、204、400、404；省略port默认49983。已有泛域名Host路由保持优先，不能被header改投其他sandbox。受限公网流量sandbox仍需其正常traffic access token，缺token的403与路由失败分开记录，不关闭访问控制。

额外用官方E2B SDK设置`E2B_SANDBOX_URL`为同一固定地址，确认实际命令和文件操作都正常；仅`/health`不能验证完整SDK链路。现有用例是`tests/e2e/sdk_compat/cases/network/test_e2b_header_routing.py`，缺`CUBE_PROXY_NODE_IP`会SKIP。

### L1：pause/restore连接和恢复尾延迟（#1925）

分别用xfs模板和s3模板运行；先单沙箱20轮，再在资源允许时用5/10个独立沙箱并发。每个2GB模板的10并发至少规划20GB沙箱内存及额外存储余量。不要复用原提交的毫秒数作跨机器硬阈值。

```bash
python3 - <<'PY'
import os
import time
from cubesandbox import Sandbox

sb = Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"])
print("sandbox_id=", sb.sandbox_id, flush=True)
sb.files.write("/tmp/repro-pause.txt", "pause-data-v1")
for i in range(20):
    before = time.monotonic()
    sb.pause(timeout=120)
    paused = time.monotonic()
    sb = Sandbox.connect(sb.sandbox_id)
    resumed = time.monotonic()
    r = sb.commands.run("cat /tmp/repro-pause.txt; printf '\\ncycle-ok\\n'")
    print(i, "pause_ms=", round((paused-before)*1000, 1),
          "resume_ms=", round((resumed-paused)*1000, 1),
          "exit=", r.exit_code, "stdout=", repr(r.stdout), flush=True)
    assert r.exit_code == 0 and "pause-data-v1" in r.stdout and "cycle-ok" in r.stdout
sb.kill()
PY
```

SDK pause默认带状态轮询，测得的是SDK端到端时间，不能直接与提交中的req-side latency比较。记录restore后首次命令、后续命令、日志转发、reset/reconnect错误，以及约3秒尾部的出现频率。健康版本不应持续出现空stdout、死socket或重试后不能执行；保留每轮原始时间。

## 5. 第二批：失败、重试及引用一致性

### R1：AppSnapshot rootfs已存在 / 半成品重试

先区分**同artifact的节点重试**和**full redo重建新的artifact**，它们不是同一个失败路径。

```bash
cm tpl redo --template-id "$CUBE_TEMPLATE_ID" --node "$NODE_B" \
  | tee "$EVIDENCE/retry-1.log"
cm tpl redo --template-id "$CUBE_TEMPLATE_ID" --node "$NODE_B" \
  | tee "$EVIDENCE/retry-2.log"
cm tpl redo --template-id "$CUBE_TEMPLATE_ID" --failed-only \
  | tee "$EVIDENCE/retry-failed-only.log"
```

这里不加`--json`或`--detach`，CLI默认等待完成；上一条结束后才运行下一条，若异常先收日志。最后一条在没有失败副本时可能明确拒绝，不当作回归。重复同模板操作后验证可创建/执行，并检查catalog/rootfs generation。不要删除双`tpl-`前缀对象来“修好”结果。

**两次普通redo只证明流程可重复，不保证命中完整catalog复用。** Master可能先清理目标副本及catalog；该细分分支要求存在合法残留且未被前置清理删除，没有CLI故障注入flag。

半成品没有安全通用的一键生产注入命令，优先在隔离的Linux builder运行当前`appsnapshot_retry_test.go`：完整catalog复用；不同fingerprint拒绝；不完整catalog清理；S3半成品清理；无catalog但已有published对象拒绝；只清理当前请求范围。

真实故障组在专用节点、已定位snapshot写入阶段后中断单次构建；保留catalog/对象状态，重启后redo。**不可盲杀共享节点的Cubelet或CubeS3lvol**。通过标准是仅清理自己拥有的半成品，不破坏仍被引用的完整对象；后续重试不再撞`rootfs already exists`，且内容正确。

### R2：#2063 full redo新artifact与definition旧artifact

原始复现：https://github.com/TencentCloud/CubeSandbox/issues/2063。

1. 使用容易在artifact生成后、`CREATING_TEMPLATE`阶段启动失败的测试镜像/环境。issue的嵌套虚拟化健康启动超时是概率触发，不保证一次命中。
2. 必须先确认首次job已经记录artifact A且失败；definition/cache确实存在并指向A。只失败在拉镜像之前没有覆盖该bug。
3. 不修改模板spec，按原id执行redo，直到full rebuild生成**不同artifact B**且最终READY；逐次保存job状态和3.3的SQL。
4. 在同一Master的缓存热路径执行3.2，再通过另一个Master入口执行；如需冷路径，仅重启专用Master实例，先保存旧缓存现象。
5. **旧版本特征**：job/replica=B、definition/request=A，Sandbox.create报`artifact sha256 mismatch`，或下载旧artifact失败。
6. **补丁验收**：definition FK及request中的ID/SHA/ticket_url/download_origin匹配B；每个READY副本用B；热/冷和其他Master创建均真正执行成功；别名不丢。

若redo复用A，记为“未触发full-rebuild条件”，不是#2063通过。若需要确定性辅助，当前纯SQLite的`TestEnsureTemplateDefinitionReplacesRebuiltArtifact`等测试可直接构造A→B，不依赖概率性VM故障；但它不等价真实CLI+TC端到端复现。

不要手工UPDATE定义、清缓存或删A后再收证据，那会掩盖根因。修改前后的工作区补丁与部署版本必须分别记录。

### R3：#2078 同规格新构建卡BUILT

原始复现：https://github.com/TencentCloud/CubeSandbox/issues/2078。

1. 创建一个已验证能生成artifact、但健康probe必失败的spec，例如基线服务真实端口49983，probe path改为一个确认返回404的路径。
2. A失败后立即用**相同image、backend、资源、env、probe**提交新的create，得到模板/job B；不要以换alias代表已换fingerprint，核对DB中的fingerprint。
3. 保存旧artifact处于CLEANUP_PENDING的证据、新B的BUILT/result持久化、12次callback/report重试耗尽和最终错误。
4. 继续按时间点0、1、5、10、15、20分钟采样，观察GC及BUILT reconciler是否重放。若CLEANUP_PENDING很快清完，记录“阻塞窗口未命中”。
5. 当前默认BUILT扫描阈值约15分钟、周期约5分钟；**600秒仍BUILT不能单独证明永久卡死**。实际`CUBE_MASTER_BUILT_STALE_AFTER`和reconcile配置必须记录。
6. 通过：保持durable构建结果，有效artifact最终收敛或得到明确诊断；若复用旧READY winner，也必须验证字节确实存在、SHA匹配。

这是独立生命周期问题，#2063更新发生在artifact注册之后，不能保证解除此前的fingerprint阻塞。不要强删旧行/locator、强改CLEANUP_PENDING为BUILDING或用任意超时直接丢掉BUILT结果。

### N1：真正退役节点与暂时不可达的对照（#1965）

建立仅有专用节点B副本的模板，保存replica/placement，然后分两组：

- **暂时不可达**：停止B的Cubelet但保留节点注册，尝试模板删除；必须保留无法确认的清理轨迹，不能据一次RPC失败宣称节点永久退役。先恢复服务再进入下一组。
- **真正退役**：按节点操作指南隔离B，等至少60秒，清空测试沙箱，停止Cubelet避免立即重新注册，再删除节点记录：

```bash
cubeopscli node isolate "$NODE_B"
# 等待、确认没有其他业务、清空本轮沙箱后，在B停止Cubelet。
cubeopscli node rm "$NODE_B"
cubeopscli node list --json
cm tpl delete "$CUBE_TEMPLATE_ID"
```

`node rm`要求隔离且无沙箱；离线节点的force行为见[节点操作](../guide/node-operations.md)，不默认绕过检查。模板删除后核对definition/replica/placement与物理清理计划；node registry未知不能当“所有节点都退役”。

另一组保留正常节点A，再退役B：模板只应根据当前有效副本报告状态；A仍可用不能被误删。全部有效副本消失后不能继续虚假READY。最后恢复B需启动Cubelet重新注册，确认状态后再投入测试；删除记录不是永久禁用。

### L2：删除模板不能破坏它的snapshot（#1678/#1763）

1. 创建专用模板T；从T创建sandbox，写一份随机文件，记录SHA256，调用`create_snapshot()`保存snapshot ID。
2. kill源sandbox，让live sandbox不再是引用阻止条件；snapshot仍保留。
3. 使用`cm tpl delete T`删除源模板。
4. `Sandbox.create(template=snapshot_id)`新建并读取文件，比较SHA256及实际stdout。
5. 删除源模板可以明确拒绝引用中的artifact清理，或只删除模板并保留snapshot引用的artifact；不能删除成功后使snapshot恢复报sha mismatch/404。
6. 最后kill恢复出的sandbox，再通过`Sandbox.delete_snapshot(snapshot_id)`释放snapshot，观察artifact的最后引用清理。

参考现成`examples/snapshot-rollback-clone/01_create_snapshot.py`、`03_clone_from_snapshot.py`、`05_snapshot_outlives_sandbox.py`。**“删snapshot后已有sandbox继续存活”是另一条测试，不能替代本用例。**

## 6. 最后一批：S3lvol与重启，只在专用环境运行

### S1：#2077 exports 256边界

原始复现：https://github.com/TencentCloud/CubeSandbox/issues/2077。

1. 确认CoW=s3，使用专用lvstore/bucket/prefix；记录当前exports、imports、checkpoint、WAL和正常attach日志。
2. 在同一运行sandbox上按每批10次`create_snapshot()`逐步增加并**保留**snapshot。每批核对实际exports数量，而不是假设1个snapshot=1个export。
3. exports读数通过实际lvstore对象key获取；与artifact bucket的`object_key`不是同一个key：

```bash
export S3_ENDPOINT='<AWS CLI使用的http(s)对象存储endpoint>'
export S3LVOL_BUCKET='<专用lvstore bucket>'
export S3LVOL_EXPORTS_KEY='<实际lvstore前缀>/meta/exports.json'
aws --endpoint-url "$S3_ENDPOINT" s3 cp \
  "s3://$S3LVOL_BUCKET/$S3LVOL_EXPORTS_KEY" - \
  | jq '{version, export_count: (.exports | length)}'
```

4. 先验证255/256附近的clean detach/attach；然后增加至至少257，记录JSON有效、实际entry数和加载错误。
5. 在独占环境通过正常卸载/重启CubeS3lvol再attach；禁止清理或重新初始化原prefix来“恢复”。
6. 旧代码特征：写入继续增长，但attach解码固定256数组失败，重启仍失败。修复验收需257/512等边界roundtrip与全部snapshot数据可读；提高常量不是无上限修复。

越界后可能使整个lvstore不可用。测试前保存受控备份和恢复计划，失败后保留原exports/WAL/checkpoint，不手工截断exports去掉snapshot依赖。

### S2：#1971 WAL与#2091计算节点重启

二者分开编号记录：WAL回放错序、进程SIGKILL、整机重启不是同一覆盖。

**基础recovery脚本**，仅在无业务、无其他target进程的Linux测试机，依赖已构建的新二进制：

```bash
cd CubeS3lvol
test/run_all.sh --list
sudo -E test/dataplane/run_recovery_test.sh \
  -e '<脚本要求的endpoint主机名>' -b '<专用测试bucket>' -r '<region>'
```

凭证走受控AWS环境，不写在命令参数或文档。脚本验证clean unload/attach、存在unflushed overlay时SIGKILL、随后A/B数据恢复；可能写固定`/data`状态和NVMe资源，失败会保留诊断prefix，重跑前先查报告，不能直接删除所有残留。

**#1971 专项条件**：checkpoint后必须还保留可解码的旧WAL记录，序号混合`<=ckpt_seq`和`>ckpt_seq`，覆盖旧WRITE/UNMAP以及跨batch边界。重放不得让旧WRITE覆盖新metadata或旧UNMAP删除新mapping。现有普通recovery脚本通过不等于命中本条件；需要补定向WAL故障用例，再用snapshot/pause/checkpoint/重启的SHA256结果验收。不要手改真实WAL注入记录。

**#2091 整机重启**：在s3 sandbox中写文件并记录SHA，创建并确认remote-ready snapshot或pause包，记录sandbox/snapshot/node/namespace ID；隔离并清空其他业务后，按测试机运维流程重启计算节点。启动后记录systemd顺序、lvstore attach、exports/imports、NVMe namespace、metadata remount、sandbox控制面状态。

按API恢复原sandbox或从已有snapshot新建，真实读文件并核对SHA。若版本不支持自动恢复原running VM，区分“产品恢复行为”与“残留对象阻塞新建”；不能用snapshot可恢复证明原running VM自动恢复成功，也不能用SIGKILL单进程证明整机重启通过。

## 7. 补充：镜像ENV与失败Resume

### E1：#2064 ENV合并

准备基线镜像中至少`REPRO_BASE=base`、`REPRO_KEEP=keep`两项环境，确认不加override创建时能读到。使用同镜像创建模板，加`--env REPRO_BASE=override --env REPRO_EXTRA=extra`。

在其sandbox中执行：

```bash
printenv REPRO_BASE REPRO_KEEP REPRO_EXTRA
```

预期依次override、keep、extra；再snapshot/from-snapshot重复。丢REPRO_KEEP是“替换全部ENV”的证据。不要用create-time `env_vars`的正常工作替代模板构建ENV合并验证；LANG/LC_ALL也只在镜像原本明确存在时断言。

### #1847 / #2000 后续专项

- #1847：需要在rootfs派生成功后、memory导入或shim启动失败的位置注入单次失败；不删除pause源包，再次connect同sandbox。核对自己新建的CoW对象/metadata/memory被回滚，重试不撞EEXIST，最后文件SHA正确。失败后不先kill或手删rootfs来掩盖孤儿。
- #2000：需要确认具体stale S3 metadata/index对象，在保留相应index、缺失其base metadata的受控夹具中重建；普通chunk cache、UUID转换单测不代表此问题已复现。
- 这两项当前没有本文已验证的一键故障注入；先收清楚失败阶段和ID，再共同补最小夹具，不能将未触发记PASS。

## 8. 开发侧辅助测试（不是端到端结果）

从仓库根目录，Linux builder/Docker依赖准备好后，可按层运行；本文只提供命令，不已执行这些故障场景：

```bash
make cubemaster-test
make cubetemplatecenter-test
make cubelet-test
make cube-s3lvol-test
```

聚焦D3/R1的包与现有用例：

- Master `pkg/templatecenter`：`TestArtifactDownloadTTL`、`TestBuildArtifactDownloadTicket*`、`TestEnsureTemplateDefinition*`、`TestTemplateArtifact*`、`TestSnapshotArtifactLifecycle*`、`TestListTemplatesWithOptions*`；`*`在此表示名称前缀，使用Go `-run`时写正则，先`go test -list`确认选中了用例。
- TC `pkg/httpservice`：`TestArtifactDownloadHandlerValidatesTokenIdentityAndGeneration`、`TestDirectArtifactPathConstrainsFSObjectKey`。
- Cubelet `internal/cube/server/images/ext4image`：D3列出的6个用例；`services/cubebox`：`TestReconcileAppSnapshotRetry*`。
- CubeS3lvol：先`make check-offline`，再在独占真实S3环境按README运行dataplane；exit 2 / SKIP单列。

macOS的gomonkey会受代码页写权限限制，TC/Cubelet还有Linux syscall/native库依赖；不要以交叉编译成功冒充执行。SQLite用例不能证明MySQL/PostgreSQL真实行锁。上一轮主代理在本机完成的回归、race、vet仅作为已有开发侧证据，不替代本文逐场景复现。

## 9. 回传结果与清理

每个场景填写一行，失败保留原始时间线：

| 场景 | 基线/补丁及组件版本 | 拓扑/artifact/CoW backend | template/job/artifact/generation/node/sandbox/snapshot ID | 操作与UTC时间 | 预期 | 实际/错误码 | 证据文件 | 结论 |
|---|---|---|---|---|---|---|---|---|
| D1 | 待填 | 待填 | 待填 | 待填 | 直连S3+SHA正确 | 待填 | 待填 | 未执行 |
| R2 | 待填 | 待填 | 待填 | 待填 | A→B一致+真实可执行 | 待填 | 待填 | 未执行 |
| S1 | 待填 | 待填 | 待填 | 待填 | >256可attach且数据可读 | 待填 | 待填 | 未执行 |

共享前脱敏：AK/SK、callback/signing key、refresh/download token、S3 URL query签名、JWT、业务文件内容。流量证据保留来源主机/端口/字节统计即可。

先收证据再清理：仅kill本轮sandbox、删除本轮snapshot和专用template；恢复测试中隔离的节点及网络规则。S3lvol失败前缀按报告逐个处理，先确认无其他引用，不使用bucket级清空、全namespace删除、全目录rm或手工删DB行。

建议一起复现的顺序：**D1/D2 → D3/B1/P1/L1/L3/E1 → R1/R2/R3/L2/N1 → S1/S2**。先把artifact直下载与CoW恢复问题拆开，命中条件和数据证据齐全后再解决对应根因。

## 10. 当前实现与命令依据

- 最近提交：`f80e1c2c`、`74207466`、`95510d23`、`b3f71c98`。
- [模板可靠性历史设计草稿](../../dev/template-reliability-fix-plan.md)（只作需求背景）。
- [节点操作](../guide/node-operations.md)、[跨机快照](../guide/cross-node-snapshot.md)、[快照/回滚/克隆](../guide/snapshot-rollback-clone.md)。
- CLI：`CubeMaster/cmd/cubemastercli/commands/cubebox/template.go`。
- 下载协议：`CubeMaster/pkg/templatecenter/artifact_download_ticket.go`、`CubeTemplateCenter/pkg/httpservice/artifact_download.go`、`Cubelet/internal/cube/server/images/ext4image/utils.go`、`pkgs/proto/artifactticket/ticket.go`。
- 重试：`Cubelet/services/cubebox/appsnapshot_retry_test.go`；#2063补丁：`CubeMaster/pkg/templatecenter/store.go`及`artifact_lifecycle_test.go`。
- S3lvol环境与清理语义：[测试说明](../../../CubeS3lvol/test/README.md)。
