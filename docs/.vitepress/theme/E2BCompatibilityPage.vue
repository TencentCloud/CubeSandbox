<script setup>
import { computed, ref } from 'vue'
import './e2b-compat.css'

const props = defineProps({ locale: { type: String, default: 'en' } })

const copy = {
  en: {
    heroLabel: 'SDK COMPATIBILITY MATRIX',
    lead: 'Same workflow, not identical behavior.',
    intro: 'Compare public SDK methods by name, see what works unchanged, and find the Cube alternative when behavior differs.',
    heroMeta: { result: 'Result', releases: 'Tested releases', latest: 'Latest validated', recommended: 'Recommended' },
    quickstartTitle: 'Python SDK quick start',
    quickstartIntro: 'Use the E2B Python SDK for existing E2B code, or the native CubeSandbox Python SDK for Cube APIs. Install and configure them as follows:',
    versionLabel: '01 · VERSIONS',
    versionTitle: 'Every E2B release, tested',
    versionLead: 'Each E2B Python SDK release from 1.0.0 through 2.62.0 was run against the full compatibility suite. Hover a block for its verdict; the bands below explain every judgment.',
    legend: { pass: 'Pass', partial: 'Partial', fail: 'Incompatible' },
    bandCols: ['Range', 'Verdict', 'Suite', 'Releases', 'Notes'],
    bandVerdicts: { ok: 'Pass', warn: 'Partial', bad: 'Incompatible' },
    matrixLabel: '02 · API MATRIX',
    matrixTitle: 'SDK API matrix',
    matrixIntro: 'Compatibility is assessed at the public method level. A matching name does not imply matching defaults, return types, or side effects.',
    matrixVersionWarning: 'All 156 releases were tested. Pin e2b 2.15.0 or later and everything passes; for run_code, pair the interpreter with a 2.37.1 or older core.',
    search: 'Search an API, such as connect or files.write',
    all: 'All APIs',
    api: 'SDK API',
    purpose: 'What it does',
    status: 'Status',
    difference: 'Behavior and differences',
    noResults: 'No APIs match this filter.',
    example: 'View example',
    hideExample: 'Hide example',
    copyCode: 'Copy',
    copiedCode: 'Copied',
    exampleNote: 'Examples run against Cube with a validated E2B Python SDK. Set E2B_API_KEY and CUBE_TEMPLATE_ID first. "Not available" entries show calls Cube does not implement.',
    apiLabel: '03 · CUBE API',
    apiTitle: 'REST API compatibility',
    apiIntro: 'One level below the SDK: e2b client endpoints against CubeAPI paths, parameters, responses, and error codes.',
    apiGroups: { sandboxes: 'Sandboxes', snapshots: 'Snapshots', templates: 'Templates', volumes: 'Volumes', envd: 'ENVD data plane' },
    apiStatuses: { ok: 'Aligned', warn: 'Differs', bad: 'Incompatible', missing: 'Missing', ext: 'Cube only' },
    envdNote: 'The data plane bypasses CubeAPI: CubeProxy forwards straight to envd inside the sandbox (compiled from upstream e2b-dev/infra), so request bodies, streaming frames, and error frames are identical; fixed-address traffic is routed via E2b-Sandbox-* headers.',
    sourceTitle: 'Pinned sources',
    sourceIntro: 'The matrix is based on implementation and machine-readable API definitions, not product-name similarity.',
    footNote: 'Aggregates only · verified against pinned releases',
    statuses: { compatible: 'Compatible', partial: 'Partial', mapped: 'Alternative', missing: 'Not available', extension: 'Cube extension' },
    categories: { lifecycle: 'Sandbox lifecycle', runtime: 'Commands, files & PTY', storage: 'Snapshots, volumes & templates', platform: 'Platform services', extension: 'Cube extensions' },
    versionNotes: [
      'A few releases fail a small number of cases; each was checked case by case, and every remaining failure comes from the SDK itself or the test environment, never from Cube.',
      'run_code needs a matching e2b-code-interpreter: cores 2.38+ do not pair with interpreter 2.9.0, so use a 2.37.1 or older core.'
    ],
    bands: [
      { range: '2.15.0 – 2.62.0', verdict: 'ok', suite: 'All passed', count: '72', why: 'The full suite passes.' },
      { range: '2.6.0 – 2.14.1', verdict: 'ok', suite: 'Partially passed', count: '23', why: 'Passes after triage — the remaining failures are features those SDK versions lack, not platform issues.' },
      { range: '1.6.0 – 1.11.1', verdict: 'warn', suite: 'Partially passed', count: '8', why: 'Create, commands, files, and queries work; those SDK lines simply lack pause, network, and other newer options.' },
      { range: '2.0.0 – 2.5.0', verdict: 'bad', suite: 'Failed', count: '29', why: 'The suite does not pass on this range. Use 2.6.0 or later.' },
      { range: '1.0.0 – 1.5.6', verdict: 'bad', suite: 'Failed', count: '24', why: 'The suite does not pass on this range. For basics only, 1.6.0+ works.' }
    ]
  },
  zh: {
    heroLabel: 'SDK 兼容性矩阵',
    heroMeta: { result: '测试结果', releases: '实测版本', latest: '最新验证', recommended: '推荐版本' },
    quickstartTitle: 'Python SDK 快速上手',
    quickstartIntro: '已有 E2B 代码可以使用 E2B Python SDK，也可以使用 CubeSandbox 原生 Python SDK。安装并配置以下环境变量即可：',
    versionLabel: '01 · 版本',
    versionTitle: 'E2B版本兼容性',
    versionLead: '从 1.0.0 到 2.62.0 的每个 E2B Python SDK 版本都跑了完整的兼容性测试套件。',
    legend: { pass: '通过', partial: '基本通过', fail: '不兼容' },
    bandCols: ['版本区间', '判定', '套件通过', '版本数', '说明'],
    bandVerdicts: { ok: '通过', warn: '基本通过', bad: '不兼容' },
    matrixLabel: '02 · API 对比',
    matrixTitle: 'SDK API',
    matrixIntro: '按公开方法判断兼容性。同名方法不代表默认值、返回类型和副作用完全一致。',
    search: '搜索 API，例如 connect 或 files.write',
    all: '全部 API',
    api: 'SDK API',
    purpose: 'API 作用',
    status: '兼容状态',
    difference: '行为与差异说明',
    noResults: '没有符合当前条件的 API。',
    example: '查看示例',
    hideExample: '收起示例',
    copyCode: '复制',
    copiedCode: '已复制',
    apiLabel: '03 · Cube API',
    apiTitle: 'REST API 兼容性',
    apiIntro: 'SDK 之下的协议层：逐端点对比 e2b 客户端与 CubeAPI 的路径、入参、响应与错误码。',
    apiGroups: { sandboxes: '沙箱', snapshots: '快照', templates: '模板', volumes: 'Volume', envd: 'ENVD 数据面' },
    apiStatuses: { ok: '一致', warn: '有差异', bad: '不兼容', missing: '缺失', ext: 'Cube 独有' },
    envdNote: '数据面不经 CubeAPI：CubeProxy 直接转发到沙箱内的 envd（编译自上游 e2b-dev/infra），请求体、流式分片与错误帧完全一致；固定地址流量通过 E2b-Sandbox-* 头路由。',
    sourceTitle: '固定版本的依据',
    sourceIntro: '本矩阵依据双方实现和机器可读 API 定义，而不是仅根据产品名称或同名方法判断。',
    footNote: '只放汇总 · 逐版实测',
    statuses: { compatible: '兼容', partial: '部分兼容', mapped: '有替代方案', missing: '暂不支持', extension: 'Cube 扩展' },
    categories: { lifecycle: '沙箱生命周期', runtime: '命令、文件与 PTY', storage: '快照、Volume 与模板', platform: '平台服务', extension: 'Cube 扩展能力' },
    bands: [
      { range: '2.15.0 – 2.62.0', verdict: 'ok', suite: '全部通过', count: '72', why: '整套用例全部通过。' },
      { range: '2.6.0 – 2.14.1', verdict: 'ok', suite: '部分通过', count: '23', why: '逐项核对后判为通过——剩余失败都是这些 SDK 版本自身缺少的能力，不是平台问题。' },
      { range: '1.6.0 – 1.11.1', verdict: 'warn', suite: '部分通过', count: '8', why: '创建、命令、文件、查询均可正常使用；这些版本自身没有暂停、网络等新参数。' },
      { range: '2.0.0 – 2.5.0', verdict: 'bad', suite: '未通过', count: '29', why: '该区间未通过测试，建议使用 2.6.0 及以上版本。' },
      { range: '1.0.0 – 1.5.6', verdict: 'bad', suite: '未通过', count: '24', why: '该区间未通过测试。仅需基础能力时，1.6.0 及以上可用。' }
    ]
  }
}

const setupCommands = `pip install "e2b==2.29.5"
pip install cubesandbox

export CUBE_API_URL="http://127.0.0.1:3000"
export CUBE_API_KEY="e2b_000000"
export CUBE_TEMPLATE_ID="tpl-xxx"
export E2B_API_URL="http://127.0.0.1:3000"
export E2B_API_KEY="e2b_000000"`

/* E2B Python SDK releases run against the 195-case compatibility suite.
   Each entry is [version, raw passed count, verdict: 'ok' | 'partial' | 'no']. */
const sdkVersions = [['1.0.0',134,'no'],['1.0.1',134,'no'],['1.0.2',134,'no'],['1.0.3',134,'no'],['1.0.4',134,'no'],['1.0.5',134,'no'],['1.0.6',134,'no'],['1.1.0',134,'no'],['1.2.0',134,'no'],['1.2.1',134,'no'],['1.3.0',134,'no'],['1.3.1',133,'no'],['1.3.2',133,'no'],['1.3.3',133,'no'],['1.3.4',133,'no'],['1.3.5',133,'no'],['1.4.0',133,'no'],['1.5.0',134,'no'],['1.5.1',134,'no'],['1.5.2',134,'no'],['1.5.3',134,'no'],['1.5.4',134,'no'],['1.5.5',134,'no'],['1.5.6',134,'no'],['1.6.0',170,'partial'],['1.7.0',170,'partial'],['1.7.1',170,'partial'],['1.8.0',171,'partial'],['1.9.0',171,'partial'],['1.10.0',172,'partial'],['1.11.0',173,'partial'],['1.11.1',173,'partial'],['2.0.0',175,'no'],['2.0.1',175,'no'],['2.0.2',175,'no'],['2.1.0',175,'no'],['2.1.1',175,'no'],['2.1.2',175,'no'],['2.1.3',175,'no'],['2.1.4',175,'no'],['2.1.5',175,'no'],['2.2.0',175,'no'],['2.2.1',175,'no'],['2.2.2',174,'no'],['2.2.3',174,'no'],['2.2.4',175,'no'],['2.2.5',174,'no'],['2.2.6',174,'no'],['2.2.7',175,'no'],['2.3.0',175,'no'],['2.3.1',175,'no'],['2.3.2',175,'no'],['2.3.3',175,'no'],['2.3.4',174,'no'],['2.3.5',175,'no'],['2.3.6',174,'no'],['2.4.0',175,'no'],['2.4.1',175,'no'],['2.4.2',174,'no'],['2.4.3',174,'no'],['2.5.0',175,'no'],['2.6.0',174,'ok'],['2.6.1',175,'ok'],['2.6.2',174,'ok'],['2.6.3',175,'ok'],['2.6.4',174,'ok'],['2.7.0',181,'ok'],['2.8.0',183,'ok'],['2.8.1',183,'ok'],['2.9.0',183,'ok'],['2.9.1',183,'ok'],['2.10.0',183,'ok'],['2.10.1',182,'ok'],['2.10.2',182,'ok'],['2.11.0',183,'ok'],['2.12.0',182,'ok'],['2.12.1',180,'ok'],['2.12.2',181,'ok'],['2.13.0',182,'ok'],['2.13.1',183,'ok'],['2.13.2',182,'ok'],['2.13.3',182,'ok'],['2.14.0',182,'ok'],['2.14.1',183,'ok'],['2.15.0',195,'ok'],['2.15.1',195,'ok'],['2.15.2',195,'ok'],['2.15.3',195,'ok'],['2.16.0',195,'ok'],['2.17.0',195,'ok'],['2.18.0',195,'ok'],['2.19.0',195,'ok'],['2.20.0',195,'ok'],['2.20.1',195,'ok'],['2.20.2',195,'ok'],['2.20.3',195,'ok'],['2.21.0',195,'ok'],['2.21.1',195,'ok'],['2.22.0',195,'ok'],['2.23.0',195,'ok'],['2.23.1',195,'ok'],['2.24.0',195,'ok'],['2.25.0',195,'ok'],['2.25.1',195,'ok'],['2.26.0',195,'ok'],['2.27.0',195,'ok'],['2.27.1',195,'ok'],['2.28.0',195,'ok'],['2.28.1',195,'ok'],['2.28.2',195,'ok'],['2.29.0',195,'ok'],['2.29.1',195,'ok'],['2.29.2',195,'ok'],['2.29.3',195,'ok'],['2.29.4',195,'ok'],['2.29.5',195,'ok'],['2.29.6',195,'ok'],['2.30.0',195,'ok'],['2.31.0',195,'ok'],['2.32.0',195,'ok'],['2.32.1',195,'ok'],['2.33.0',195,'ok'],['2.34.0',195,'ok'],['2.35.0',195,'ok'],['2.36.0',195,'ok'],['2.37.0',195,'ok'],['2.37.1',195,'ok'],['2.38.0',195,'ok'],['2.39.0',195,'ok'],['2.39.1',195,'ok'],['2.40.0',195,'ok'],['2.41.0',195,'ok'],['2.42.0',195,'ok'],['2.43.0',195,'ok'],['2.44.0',195,'ok'],['2.45.0',195,'ok'],['2.45.1',195,'ok'],['2.46.0',195,'ok'],['2.46.4',195,'ok'],['2.48.0',195,'ok'],['2.49.0',195,'ok'],['2.49.1',195,'ok'],['2.50.0',195,'ok'],['2.51.0',195,'ok'],['2.52.0',195,'ok'],['2.52.1',195,'ok'],['2.53.1',195,'ok'],['2.55.0',195,'ok'],['2.55.1',195,'ok'],['2.56.0',195,'ok'],['2.57.0',195,'ok'],['2.58.0',195,'ok'],['2.59.0',195,'ok'],['2.60.0',195,'ok'],['2.61.0',195,'ok'],['2.62.0',195,'ok']]

const WALL_TONE = { ok: '', partial: 'partial', no: 'fail' }

/* REST-level comparison of the e2b client endpoints against CubeAPI.
   status: 'ok' aligned · 'warn' differs · 'bad' incompatible · 'missing' no route · 'ext' Cube only */
const cubeApi = [
  {
    key: 'sandboxes',
    endpoints: [
      { m: 'POST', path: '/v2/sandboxes', status: 'warn', en: 'Missing secure/iam inputs; the response is otherwise aligned (alias included).', zh: '缺 secure/iam 入参，其余一致（响应含 alias）。' },
      { m: 'GET', path: '/v2/sandboxes', status: 'warn', en: 'state/metadata filters work; order, start-time, and template filters are missing.', zh: '支持 state/metadata 筛选；缺排序、开始时间和模板筛选。' },
      { m: 'GET', path: '/sandboxes/{id}', status: 'ok', en: 'All required fields present (endAt, alias, disk size).', zh: '必填字段齐全（endAt、alias、磁盘大小）。' },
      { m: 'DELETE', path: '/sandboxes/{id}', status: 'warn', en: 'Same route; e2b returns False on 404 while Cube raises not-found.', zh: '路由一致；404 时 e2b 返回 False、Cube 抛异常。' },
      { m: 'POST', path: '/v2/sandboxes/{id}/connect', status: 'warn', en: 'Missing on_resume — restore always resumes the saved state.', zh: '缺 on_resume 参数；恢复固定回到保存时的状态。' },
      { m: 'POST', path: '/sandboxes/{id}/pause', status: 'bad', en: 'keep_memory=false is rejected — pause always keeps the memory snapshot.', zh: 'keep_memory=false 被拒绝；暂停总是保留内存。' },
      { m: 'POST', path: '/sandboxes/{id}/timeout', status: 'ok', en: 'Identical; -1 means never timing out.', zh: '完全一致；-1 表示永不超时。' },
      { m: 'POST', path: '/sandboxes/{id}/fork', status: 'ok', en: 'Identical per-fork results from one snapshot.', zh: '一致；一次快照派生多份，逐个返回结果。' },
      { m: 'POST', path: '/sandboxes/{id}/snapshots', status: 'ok', en: 'Aligned: memory=false persists the filesystem only (mapped to Cube\'s fs-only); the default keeps memory.', zh: '已对齐：memory=false 只保存文件系统（映射为 Cube 的 fs-only），默认保留内存。' },
      { m: 'PUT', path: '/sandboxes/{id}/network', status: 'warn', en: 'Missing egress_proxy and https_ports.', zh: '缺 egress_proxy、https_ports 参数。' },
      { m: 'GET', path: '/sandboxes/{id}/metrics', status: 'missing', en: 'No such route; use Cubelet metrics instead.', zh: '无此路由；请使用 Cubelet 指标。' },
      { m: 'POST', path: '/sandboxes/{id}/resume', status: 'ext', en: 'Cube only: explicit resume.', zh: 'Cube 独有：显式恢复。' },
      { m: 'POST', path: '/sandboxes/{id}/rollback', status: 'ext', en: 'Cube only: in-place rollback.', zh: 'Cube 独有：原地回滚。' },
      { m: 'POST', path: '/sandboxes/{id}/clone', status: 'ext', en: 'Cube only: clone a sandbox.', zh: 'Cube 独有：克隆沙箱。' }
    ]
  },
  {
    key: 'snapshots',
    endpoints: [
      { m: 'GET', path: '/snapshots', status: 'ok', en: 'Name filter and paging work.', zh: '支持按名称过滤，分页一致。' },
      { m: 'DELETE', path: '/templates/{id or name}', status: 'ok', en: 'Deletes a snapshot by ID or name, returns bool; ambiguous names get 409.', zh: '按 ID 或名称删除快照，返回 bool；名称歧义时返回 409。' },
      { m: 'DELETE', path: '/templates/{id}', status: 'ok', en: 'Template deletion aligned.', zh: '模板删除一致。' }
    ]
  },
  {
    key: 'templates',
    endpoints: [
      { m: 'GET', path: '/templates', status: 'warn', en: 'Flat list; missing public/builds fields.', zh: '返回扁平列表；缺 public/builds 字段。' },
      { m: 'GET', path: '/templates/aliases/{alias}', status: 'bad', en: 'Missing the aliases field; the e2b model fails to parse.', zh: '缺 aliases 字段，e2b 模型解析失败。' },
      { m: 'GET', path: '/templates/{id}', status: 'ok', en: 'All fields present.', zh: '字段齐全。' },
      { m: 'POST', path: '/templates', status: 'bad', en: 'Different build models: e2b takes a Dockerfile DSL, Cube takes an image reference.', zh: '构建模型不同：e2b 收 Dockerfile DSL，Cube 收镜像引用。' },
      { m: 'GET', path: '/v2/templates', status: 'missing', en: 'No v2 template routes (tags).', zh: '无 v2 模板路由（tags 相关）。' },
      { m: 'POST', path: '/templates/tags', status: 'missing', en: 'No tags model.', zh: '无 tags 模型。' },
      { m: 'GET', path: '/templates/{id}/builds/{bid}/status', status: 'warn', en: 'Missing log_entries; adds progress/message.', zh: '缺 log_entries；多 progress/message。' },
      { m: 'GET', path: '/templates/{id}/builds/{bid}/logs', status: 'ext', en: 'Cube only: build logs.', zh: 'Cube 独有：构建日志。' },
      { m: 'POST', path: '/templates/{id}/rebuild', status: 'ext', en: 'Cube only: rebuild from the image.', zh: 'Cube 独有：重新构建。' },
      { m: 'PUT', path: '/templates/{id}/alias', status: 'ext', en: 'Cube only: set alias.', zh: 'Cube 独有：写入别名。' }
    ]
  },
  {
    key: 'volumes',
    endpoints: [
      { m: 'POST', path: '/volumes', status: 'ok', en: 'Aligned (name optional; response misses domain).', zh: '一致（name 可省略；响应缺 domain 字段）。' },
      { m: 'GET', path: '/volumes', status: 'ok', en: 'Aligned.', zh: '一致。' },
      { m: 'GET', path: '/volumes/{id}', status: 'ok', en: 'Aligned (misses domain).', zh: '一致（缺 domain 字段）。' },
      { m: 'DELETE', path: '/volumes/{id}', status: 'ok', en: 'Aligned; 404 returns False.', zh: '一致；404 返回 False。' },
      { m: '*', path: '/volumecontent/**', status: 'missing', en: 'The whole volume-content group (7 endpoints); mount the volume and use sandbox.files.', zh: '卷内容 API 整组缺失（7 个端点）；请先挂载，再通过 sandbox.files 访问。' }
    ]
  },
  {
    key: 'envd',
    endpoints: [
      { m: 'RPC', path: 'process / filesystem / files / execute', status: 'ok', en: 'Commands, files, PTY, and code execution are all identical.', zh: '命令、文件、PTY、代码执行全部一致。' }
    ]
  }
]

const rows = [
  { category: 'lifecycle', api: 'Sandbox.create()', status: 'partial', cube: 'Sandbox.create()', en: 'Creates sandboxes normally. Not supported: autoPauseMemory=false — Cube always keeps the memory snapshot, so the v2 flag is rejected. The E2B SDK also never reads CUBE_TEMPLATE_ID; pass the template explicitly.', zh: 'Cube 不支持：autoPauseMemory=false——Cube 始终保留内存快照，该参数会被拒绝。Cube 的默认模板环境变量为 CUBE_TEMPLATE_ID，E2B 需要显式传入模板。' },
  { category: 'lifecycle', api: 'Sandbox.connect()', status: 'partial', cube: 'Sandbox.connect(id)', en: 'The normal connect flow works unchanged. The one difference is what timeout means: E2B sets a fixed deadline (the sandbox dies at that moment, active or not), Cube sets an idle window (it is reclaimed only after N idle seconds; activity keeps it alive). connect() only extends in both SDKs — use set_timeout() to replace it. -1 is rejected by the E2B v2 API even though Cube supports never-timing-out.', zh: '连接与恢复运行完全可用。唯一差异是 timeout 的含义：E2B 是固定到期时间（到点即销毁，无论是否活跃），Cube 是空闲窗口（闲置满 N 秒才回收，活跃使用会自动续活）。connect() 两边都只能延长、不能缩短，覆盖请用 set_timeout()；-1 会被 E2B v2 接口拒绝，尽管 Cube 支持永不超时。' },
  { category: 'lifecycle', api: 'Sandbox.list()', status: 'partial', cube: 'Sandbox.list_v2()', en: 'Listing and the state/metadata filters work unchanged. The one difference is the result shape: Cube returns one plain list, so ordering and start-time/template filters are not available.', zh: '列表查询和 state/metadata 筛选都可直接使用。唯一差异是返回形式：Cube 一次性返回普通列表，排序和开始时间/模板过滤暂不可用。' },
  { category: 'lifecycle', api: 'sandbox.get_info()', status: 'compatible', cube: 'sandbox.get_info()', en: 'Core fields align, including the template name E2B reads. Cube additionally exposes disk size and finer-grained states.', zh: '核心字段一致（含 E2B 读取的模板名称）。Cube 额外提供磁盘大小、更细的状态信息。' },
  { category: 'lifecycle', api: 'sandbox.is_running()', status: 'compatible', cube: 'sandbox.is_running()', en: 'Works the same as E2B — checks whether the sandbox is currently reachable.', zh: '与 E2B 行为一致，检查沙箱当前是否仍在运行。' },
  { category: 'lifecycle', api: 'sandbox.kill()', status: 'partial', cube: 'sandbox.kill()', en: 'Destroys the sandbox. Not supported: returning False for a missing ID — Cube raises a not-found error instead.', zh: '可以销毁沙箱。不支持：ID 不存在时返回 False——Cube 会改抛 not-found 错误。' },
  { category: 'lifecycle', api: 'sandbox.pause()', status: 'partial', cube: 'sandbox.pause()', en: 'Pauses and keeps memory. Not supported: keep_memory=False (filesystem-only pause) — Cube always snapshots memory.', zh: '可以暂停并保留内存。不支持：keep_memory=False（仅暂停文件系统）——Cube 总是保存内存。' },
  { category: 'lifecycle', api: 'sandbox.set_timeout()', status: 'partial', cube: 'sandbox.set_timeout()', en: 'Replaces the current timeout. Not supported: negative values in the E2B API — Cube treats -1 as a never-expiring window, while E2B rejects it.', zh: '可以重新设置超时。不支持：E2B 接口传负数——Cube 把 -1 解释为永不过期，E2B 接口则直接拒绝。' },
  { category: 'lifecycle', api: 'sandbox.update_network()', status: 'partial', cube: 'sandbox.update_network()', en: 'Replaces the whole policy. Not supported: egress_proxy, https_ports, and IAM transforms; Cube adds ordered L7 rules instead.', zh: '可以整包替换策略。不支持：egress_proxy、https_ports 和 IAM transform；Cube 另提供有序 L7 规则。' },
  { category: 'lifecycle', api: 'sandbox.get_metrics()', status: 'missing', cube: 'Cubelet Prometheus metrics', en: 'Not implemented; Cube exports its own metrics through Cubelet.', zh: '未实现该接口；Cube 通过 Cubelet 导出自己的指标。' },
  { category: 'runtime', api: 'sandbox.commands.run()', status: 'compatible', cube: 'sandbox.commands.run()', en: 'Command, cwd, environment, user, stdout/stderr, exit code, and execution timeout work through the compatible envd process protocol.', zh: '命令、cwd、环境变量、用户、stdout/stderr、退出码和执行超时通过兼容的 envd process 协议工作。' },
  { category: 'runtime', api: 'commands.connect()', status: 'partial', cube: 'sandbox.pty.connect()', en: 'Commands work normally. Not supported: reconnecting to a background command — Cube Commands exposes run only; reconnect through PTY instead.', zh: '执行命令正常。不支持：重连后台命令——Cube Commands 只有 run，重连请改用 PTY。' },
  { category: 'runtime', api: 'commands.list / kill / send_stdin', status: 'missing', cube: 'sandbox.pty.*', en: 'Not exposed; use PTY control or commands.run instead.', zh: '未暴露这些方法；可用 PTY 控制交互进程，或用 commands.run 执行系统工具。' },
  { category: 'runtime', api: 'files.read / write / write_files', status: 'compatible', cube: 'sandbox.files.*', en: 'Common text/binary transfer and batch-write shapes are supported over the compatible filesystem protocol.', zh: '常用文本/二进制传输和批量写入通过兼容的文件系统协议提供。' },
  { category: 'runtime', api: 'files.list / exists / remove / rename / make_dir', status: 'compatible', cube: 'sandbox.files.*', en: 'The common directory and file-management workflow is aligned and covered by dual-backend tests.', zh: '常用目录和文件管理工作流已经对齐，并由双后端测试覆盖。' },
  { category: 'runtime', api: 'files.get_info()', status: 'mapped', cube: 'files.stat()', en: 'The capability exists under a different Cube Python name and returns a dict rather than E2B\'s typed object.', zh: 'Cube Python 以不同的方法名提供该能力，返回 dict，而不是 E2B 的类型化对象。' },
  { category: 'runtime', api: 'files.watch_dir()', status: 'partial', cube: 'files.watch_dir()', en: 'Streams directory changes. Not supported: the exact E2B handle lifecycle, event types, and callback API — the two SDKs differ here.', zh: '可以监听目录变化。不支持：与 E2B 完全一致的句柄生命周期、事件类型和回调 API——两个 SDK 在此不同。' },
  { category: 'runtime', api: 'sandbox.pty.*', status: 'compatible', cube: 'sandbox.pty.*', en: 'Create, connect, resize, input, kill, disconnect, and wait are available. Callback and exception types remain SDK-specific.', zh: '支持 create、connect、resize、输入、kill、disconnect 和 wait；回调与异常类型仍由各 SDK 自行定义。' },
  { category: 'runtime', api: 'sandbox.git.*', status: 'mapped', cube: 'commands.run("git …")', en: 'Cube Python has no high-level Git facade. Git installed in the template can be driven through the command API.', zh: 'Cube Python 没有高层 Git facade；如果模板中安装了 Git，可以通过命令 API 调用。' },
  { category: 'runtime', api: 'sandbox.run_code()', status: 'partial', cube: 'sandbox.run_code()', en: 'Runs stateful code with rich results. Not supported: version-skewed pairings — the e2b core and e2b-code-interpreter must match.', zh: '支持有状态执行和富媒体结果。不支持：版本错配——e2b 核心与 e2b-code-interpreter 必须配对。' },
  { category: 'storage', api: 'create / list / delete_snapshot', status: 'partial', cube: 'Sandbox.*_snapshot()', en: 'Create, list, and delete work, including by name. Not supported: paginator-style listing — Cube returns a list plus a next-token tuple.', zh: '创建、查询、删除（含按名称）都可用。不支持：分页器式查询——Cube 返回列表加 next_token 二元组。' },
  { category: 'storage', api: 'sandbox.fork()', status: 'compatible', cube: 'sandbox.fork()', en: 'Native on Cube with the same semantics as E2B.', zh: '已原生支持，语义与 E2B 一致。' },
  { category: 'storage', api: 'Volume.create / connect / list / destroy', status: 'partial', cube: 'cubesandbox.Volume', en: 'CRUD and mounts work. Not supported: E2B-style driver names — Cube uses operator-installed drivers, and adds optional names and per-mount read-only mode.', zh: '增删改查与挂载可用。不支持：E2B 风格的驱动名称——Cube 使用运维方安装的驱动，另支持可选名称和单次挂载只读。' },
  { category: 'storage', api: 'volume.read_file / write_file / list', status: 'missing', cube: 'Mount, then sandbox.files', en: 'Cube has no standalone volume-content service; mount it and use sandbox.files.', zh: 'Cube 没有独立的 Volume 内容服务；请先挂载，再通过 sandbox.files 访问。' },
  { category: 'storage', api: 'Template.build() fluent API', status: 'mapped', cube: 'Cube Template / cubemastercli', en: 'Not wire-compatible; Cube builds templates from OCI images with its own tooling.', zh: '模板系统不互通；Cube 基于 OCI 镜像构建，有自己的模板工具。' },
  { category: 'platform', api: 'sandbox.get_mcp_token()', status: 'partial', cube: 'Cube SDK mcp option', en: 'Works with the Cube SDKs, which start the gateway inside the sandbox and return the URL and token. Not supported: the E2B SDK\'s own token endpoint — use the Cube SDKs for MCP.', zh: 'Cube SDK 可用：在沙箱内启动网关并返回地址和令牌。不支持：E2B SDK 自身的令牌接口——使用 MCP 请改用 Cube SDK。' },
  { category: 'platform', api: 'Sandbox.create(iam=...)', status: 'missing', cube: 'CubeEgress injection', en: 'E2B workload identity is not implemented; Cube keeps credentials outside the guest instead.', zh: '未实现 E2B workload identity；Cube 将凭据保留在 Guest 之外。' },
  { category: 'platform', api: 'Secret.*', status: 'missing', cube: 'Deployment secret store', en: 'Cube has no E2B Secrets API; secrets belong to the deployment or CubeEgress.', zh: 'Cube 没有 E2B Secrets API；密钥由部署系统或 CubeEgress 管理。' },
  { category: 'platform', api: 'AsyncSandbox / async resources', status: 'missing', cube: 'Node.js SDK / executor', en: 'Cube Python is synchronous; use the native Node.js or Go SDKs for async code.', zh: 'Cube Python 是同步 API；异步场景请使用原生 Node.js 或 Go SDK。' },
  { category: 'extension', api: 'sandbox.rollback()', status: 'extension', cube: 'sandbox.rollback()', en: 'Cube restores filesystem and memory in place while retaining the sandbox ID. E2B has snapshots and forks but no equivalent in-place rollback method.', zh: 'Cube 可在保持沙箱 ID 不变的情况下原地恢复文件系统和内存；E2B 有快照和 fork，但没有等价的原地回滚方法。' },
  { category: 'storage', api: 'Sandbox.create(template=snapshot_id)', status: 'partial', cube: 'Sandbox.create(template=snapshot_id)', en: 'Both SDKs can create a new sandbox from a snapshot ID. With E2B snapshots are always portable across the hosted fleet; with Cube, cross-node placement needs an S3-backed, remotely ready snapshot on a host-compatible node — same-node restores work with local storage.', zh: '双方都支持用快照 ID 创建新沙箱。E2B 的快照在托管集群内天然可跨节点恢复；Cube 的跨节点恢复要求快照使用 S3 后端、远端状态就绪且宿主机兼容，同节点恢复使用本地存储即可。' },
  { category: 'extension', api: 'Volume.create(driver=...) / VolumeMount(read_only=True)', status: 'extension', cube: 'cubesandbox.Volume', en: 'The shared volume workflow is E2B-compatible; these two options are Cube-only: picking an operator-installed volume driver and making a single attachment read-only.', zh: 'Volume 的增删改查与挂载是双方共有的 E2B 兼容工作流；只有这两个参数是 Cube 独有的：指定运维方安装的 Volume 驱动、把某一次挂载设为只读。' },
  { category: 'extension', api: 'sandbox.update_network(rules=...)', status: 'extension', cube: 'sandbox.update_network()', en: 'Cube adds ordered L7 rules: allow or deny by host, port, or path, plus audit and header injection.', zh: 'Cube 增加有序 L7 规则：可按主机、端口、路径放行或拒绝，并支持审计和请求头注入。' },
  { category: 'extension', api: 'Sandbox.create(distribution_scope=...)', status: 'extension', cube: 'Sandbox.create()', en: 'Cube can restrict eligible compute nodes or pin a sandbox to one node. E2B does not expose equivalent placement control in its public SDK.', zh: 'Cube 可以限制沙箱可调度的计算节点，或将其固定到单个节点；E2B 公共 SDK 没有等价的放置控制。' },
  { category: 'extension', api: 'GET /v1/metrics/resource', status: 'extension', cube: 'Cubelet Prometheus endpoint', en: 'Cubelet exports host and guest resource metrics separately for Prometheus.', zh: 'Cubelet 分别导出宿主机与 Guest 的资源指标，供 Prometheus 使用。' }
]

const purposes = {
  'Sandbox.create()': { en: 'Create a new isolated sandbox from a template.', zh: '从模板创建一个新的隔离沙箱。' },
  'Sandbox.connect()': { en: 'Attach an SDK client to an existing sandbox.', zh: '让 SDK 客户端连接到已有沙箱。' },
  'Sandbox.list()': { en: 'Page through sandboxes visible to the project.', zh: '分页查询当前项目中的沙箱。' },
  'sandbox.get_info()': { en: 'Read identity, state, resources, and timestamps.', zh: '读取沙箱身份、状态、资源和时间信息。' },
  'sandbox.is_running()': { en: 'Check whether a sandbox is currently reachable.', zh: '检查沙箱当前是否仍在运行。' },
  'sandbox.kill()': { en: 'Permanently stop and remove a sandbox.', zh: '永久停止并删除一个沙箱。' },
  'sandbox.pause()': { en: 'Persist a sandbox so it can be resumed later.', zh: '暂停并持久化沙箱，以便之后恢复。' },
  'sandbox.set_timeout()': { en: 'Replace the sandbox lifecycle timeout.', zh: '重新设置沙箱的生命周期超时时间。' },
  'sandbox.update_network()': { en: 'Atomically replace outbound network rules.', zh: '原子替换沙箱的出站网络规则。' },
  'sandbox.get_metrics()': { en: 'Read recent CPU, memory, and disk samples.', zh: '读取近期 CPU、内存和磁盘指标。' },
  'sandbox.commands.run()': { en: 'Run a shell command inside the sandbox.', zh: '在沙箱内部执行一条 Shell 命令。' },
  'commands.connect()': { en: 'Reconnect to a running background command.', zh: '重新连接正在运行的后台命令。' },
  'commands.list / kill / send_stdin': { en: 'Inspect and control running processes.', zh: '查看并控制沙箱中的运行进程。' },
  'files.read / write / write_files': { en: 'Transfer text or binary file content.', zh: '读写文本或二进制文件内容。' },
  'files.list / exists / remove / rename / make_dir': { en: 'Manage files and directory structure.', zh: '管理文件以及目录结构。' },
  'files.get_info()': { en: 'Read metadata for a file or directory.', zh: '读取文件或目录的元数据。' },
  'files.watch_dir()': { en: 'Observe filesystem changes in a directory.', zh: '监听目录中的文件系统变化。' },
  'sandbox.pty.*': { en: 'Create and control an interactive terminal.', zh: '创建并控制交互式终端。' },
  'sandbox.git.*': { en: 'Perform common Git operations in a sandbox.', zh: '在沙箱中执行常见 Git 操作。' },
  'sandbox.run_code()': { en: 'Execute stateful code and collect rich results.', zh: '执行有状态代码并获得富媒体结果。' },
  'create / list / delete_snapshot': { en: 'Create, discover, and delete persistent snapshots.', zh: '创建、查询和删除持久化快照。' },
  'sandbox.fork()': { en: 'Branch one running sandbox into multiple copies.', zh: '把一个运行中的沙箱分叉成多个副本。' },
  'Volume.create / connect / list / destroy': { en: 'Manage persistent volumes independently of sandboxes.', zh: '独立于沙箱管理持久化 Volume。' },
  'volume.read_file / write_file / list': { en: 'Access files directly through a volume service.', zh: '通过 Volume 服务直接访问其中的文件。' },
  'Template.build() fluent API': { en: 'Define and build a reusable sandbox template.', zh: '定义并构建可复用的沙箱模板。' },
  'sandbox.get_mcp_token()': { en: 'Retrieve the token for an enabled MCP gateway.', zh: '获取已启用 MCP Gateway 的访问令牌。' },
  'Sandbox.create(iam=...)': { en: 'Give a sandbox short-lived workload identity.', zh: '为沙箱配置短期工作负载身份。' },
  'Secret.*': { en: 'Create, rotate, list, and destroy project secrets.', zh: '创建、轮换、查询和删除项目密钥。' },
  'AsyncSandbox / async resources': { en: 'Use sandbox resources from async Python code.', zh: '在异步 Python 程序中使用沙箱资源。' },
  'sandbox.rollback()': { en: 'Restore a running sandbox to an earlier snapshot.', zh: '把运行中的沙箱恢复到之前的快照状态。' },
  'Sandbox.create(template=snapshot_id)': { en: 'Create or restore a sandbox from a reusable snapshot.', zh: '从可复用快照创建或恢复沙箱。' },
  'Volume.create(driver=...) / VolumeMount(read_only=True)': { en: 'Select a volume backend and control attachment mode.', zh: '选择 Volume 后端并控制单次挂载模式。' },
  'sandbox.update_network(rules=...)': { en: 'Apply advanced ordered L7 egress actions.', zh: '应用高级有序 L7 出站动作。' },
  'Sandbox.create(distribution_scope=...)': { en: 'Constrain where a sandbox may be scheduled.', zh: '限制沙箱可以被调度到哪些节点。' },
  'GET /v1/metrics/resource': { en: 'Expose native sandbox resource metrics to Prometheus.', zh: '向 Prometheus 暴露原生沙箱资源指标。' }
}

const examples = {
  'Sandbox.create()': {
    e2b: `from e2b import Sandbox

# The context manager kills the sandbox when the block exits.
with Sandbox.create(timeout=60) as sandbox:
    print(sandbox.sandbox_id)`,
    cube: `from cubesandbox import Sandbox

# The context manager kills the sandbox when the block exits.
# CUBE_TEMPLATE_ID is picked up automatically when template is omitted.
with Sandbox.create(timeout=60) as sandbox:
    print(sandbox.sandbox_id)`
  },
  'Sandbox.connect()': {
    e2b: `from e2b import Sandbox

sandbox = Sandbox.create(timeout=120)
try:
    connected = Sandbox.connect(sandbox.sandbox_id, timeout=60)
    print(connected.commands.run("echo connected").stdout)
finally:
    sandbox.kill()`,
    cube: `from cubesandbox import Sandbox

sandbox = Sandbox.create(timeout=120)
try:
    connected = Sandbox.connect(sandbox.sandbox_id, timeout=60)
    print(connected.commands.run("echo connected").stdout)
finally:
    sandbox.kill()`
  },
  'Sandbox.list()': {
    e2b: `from e2b import Sandbox

paginator = Sandbox.list(limit=10)
while paginator.has_next:
    for info in paginator.next_items():
        print(info.sandbox_id, info.state)`,
    cube: `from cubesandbox import Sandbox

# list_v2() follows the cursor and returns every matching sandbox.
sandboxes = Sandbox.list_v2()
print(len(sandboxes), "sandboxes")`
  },
  'sandbox.get_info()': {
    e2b: `from e2b import Sandbox

with Sandbox.create() as sandbox:
    info = sandbox.get_info()
    print(info.sandbox_id, info.state, info.started_at)`,
    cube: `from cubesandbox import Sandbox

with Sandbox.create() as sandbox:
    info = sandbox.get_info()
    print(info.sandbox_id, info.state, info.started_at)`
  },
  'sandbox.is_running()': {
    e2b: `from e2b import Sandbox

with Sandbox.create() as sandbox:
    print("running:", sandbox.is_running())`,
    cube: `from cubesandbox import Sandbox

with Sandbox.create() as sandbox:
    print("running:", sandbox.is_running())`
  },
  'sandbox.kill()': {
    e2b: `from e2b import Sandbox

sandbox = Sandbox.create()
sandbox_id = sandbox.sandbox_id
removed = sandbox.kill()
print(sandbox_id, removed)`,
    cube: `from cubesandbox import Sandbox

sandbox = Sandbox.create()
sandbox.kill()  # Raises not-found if the ID no longer exists.`
  },
  'sandbox.pause()': {
    e2b: `from e2b import Sandbox

sandbox = Sandbox.create()
sandbox_id = sandbox.sandbox_id
sandbox.pause()  # Preserve state for a later resume.

resumed = Sandbox.connect(sandbox_id)
print(resumed.commands.run("echo resumed").stdout)
resumed.kill()`,
    cube: `from cubesandbox import Sandbox

sandbox = Sandbox.create()
sandbox.pause()  # Preserve state for a later resume.
sandbox.resume()
print(sandbox.commands.run("echo resumed").stdout)
sandbox.kill()`
  },
  'sandbox.set_timeout()': {
    e2b: `from e2b import Sandbox

with Sandbox.create(timeout=60) as sandbox:
    # Replace the current timeout with five minutes.
    sandbox.set_timeout(300)
    print(sandbox.get_info().end_at)`,
    cube: `from cubesandbox import Sandbox

with Sandbox.create(timeout=60) as sandbox:
    # Replace the current timeout with five minutes.
    sandbox.set_timeout(300)
    print(sandbox.get_info().end_at)`
  },
  'sandbox.update_network()': {
    e2b: `from e2b import Sandbox

with Sandbox.create() as sandbox:
    # Omitted fields are cleared because this replaces the policy.
    sandbox.update_network({"deny_out": ["10.0.0.0/8"]})
    print("network policy updated")`,
    cube: `from cubesandbox import Sandbox

with Sandbox.create() as sandbox:
    # Omitted fields are cleared because this replaces the policy.
    sandbox.update_network({"deny_out": ["10.0.0.0/8"]})
    print("network policy updated")`
  },
  'sandbox.get_metrics()': {
    e2b: `from e2b import Sandbox

with Sandbox.create() as sandbox:
    sandbox.commands.run("python -c 'sum(range(1000000))'")
    for sample in sandbox.get_metrics():
        print(sample)`,
    cube: `import os
from urllib.request import urlopen

cubelet_url = os.environ.get("CUBELET_URL", "http://127.0.0.1:9998")
url = f'{cubelet_url.rstrip("/")}/v1/metrics/resource'
with urlopen(url, timeout=5) as response:
    print(response.read().decode())`
  },
  'sandbox.commands.run()': {
    e2b: `from e2b import Sandbox

with Sandbox.create() as sandbox:
    result = sandbox.commands.run(
        "python -c 'print(6 * 7)'",
        timeout=30,
    )
    print(result.stdout, result.exit_code)`,
    cube: `from cubesandbox import Sandbox

with Sandbox.create() as sandbox:
    result = sandbox.commands.run(
        "python -c 'print(6 * 7)'",
        timeout=30,
    )
    print(result.stdout, result.exit_code)`
  },
  'commands.connect()': {
    e2b: `from e2b import Sandbox

with Sandbox.create() as sandbox:
    process = sandbox.commands.run("sleep 2; echo done", background=True)
    pid = process.pid
    process.disconnect()  # The process keeps running in the sandbox.
    result = sandbox.commands.connect(pid).wait()
    print(result.stdout)`,
    cube: `from cubesandbox import PtySize, Sandbox

with Sandbox.create() as sandbox:
    handle = sandbox.pty.create(PtySize(rows=24, cols=80))
    handle.disconnect()  # The process keeps running in the sandbox.
    reconnected = sandbox.pty.connect(handle.pid)
    print(reconnected.pid)
    reconnected.kill()`
  },
  'commands.list / kill / send_stdin': {
    e2b: `from e2b import Sandbox

with Sandbox.create() as sandbox:
    process = sandbox.commands.run("cat", background=True, stdin=True)
    sandbox.commands.send_stdin(process.pid, "hello\\n")
    print([item.pid for item in sandbox.commands.list()])
    sandbox.commands.kill(process.pid)`,
    cube: `from cubesandbox import PtySize, Sandbox

with Sandbox.create() as sandbox:
    handle = sandbox.pty.create(PtySize(rows=24, cols=80))
    handle.send_stdin(b"echo hello from PTY\\n")
    print(handle.pid)
    handle.kill()`
  },
  'files.read / write / write_files': {
    e2b: `from e2b import Sandbox
from e2b.sandbox.filesystem.filesystem import WriteEntry

with Sandbox.create() as sandbox:
    sandbox.files.write("/tmp/one.txt", "one")
    sandbox.files.write_files([
        WriteEntry(path="/tmp/two.txt", data="two"),
        WriteEntry(path="/tmp/three.bin", data=b"three"),
    ])
    print(sandbox.files.read("/tmp/one.txt"))`,
    cube: `from cubesandbox import Sandbox

with Sandbox.create() as sandbox:
    sandbox.files.write("/tmp/one.txt", "one")
    sandbox.files.write_files([
        ("/tmp/two.txt", "two"),
        ("/tmp/three.bin", b"three"),
    ])
    print(sandbox.files.read("/tmp/one.txt"))`
  },
  'files.list / exists / remove / rename / make_dir': {
    e2b: `from e2b import Sandbox

with Sandbox.create() as sandbox:
    sandbox.files.make_dir("/tmp/demo")
    sandbox.files.write("/tmp/demo/a.txt", "hello")
    print(sandbox.files.exists("/tmp/demo/a.txt"))
    print([entry.name for entry in sandbox.files.list("/tmp/demo")])
    sandbox.files.rename("/tmp/demo/a.txt", "/tmp/demo/b.txt")
    sandbox.files.remove("/tmp/demo")`,
    cube: `from cubesandbox import Sandbox

with Sandbox.create() as sandbox:
    sandbox.files.make_dir("/tmp/demo")
    sandbox.files.write("/tmp/demo/a.txt", "hello")
    print(sandbox.files.exists("/tmp/demo/a.txt"))
    print([entry["name"] for entry in sandbox.files.list("/tmp/demo")])
    sandbox.files.rename("/tmp/demo/a.txt", "/tmp/demo/b.txt")
    sandbox.files.remove("/tmp/demo")`
  },
  'files.get_info()': {
    e2b: `from e2b import Sandbox

with Sandbox.create() as sandbox:
    sandbox.files.write("/tmp/info.txt", "hello")
    info = sandbox.files.get_info("/tmp/info.txt")
    print(info.name, info.type, info.path)`,
    cube: `from cubesandbox import Sandbox

with Sandbox.create() as sandbox:
    sandbox.files.write("/tmp/info.txt", "hello")
    info = sandbox.files.stat("/tmp/info.txt")  # Returns a plain dict.
    print(info)`
  },
  'files.watch_dir()': {
    e2b: `from e2b import Sandbox

with Sandbox.create() as sandbox:
    sandbox.files.make_dir("/tmp/watched")
    watcher = sandbox.files.watch_dir("/tmp/watched")
    try:
        sandbox.files.write("/tmp/watched/new.txt", "hello")
        print(watcher.get_new_events())
    finally:
        watcher.stop()`,
    cube: `from cubesandbox import Sandbox

with Sandbox.create() as sandbox:
    sandbox.files.make_dir("/tmp/watched")
    with sandbox.files.watch_dir("/tmp/watched") as watcher:
        sandbox.files.write("/tmp/watched/new.txt", "hello")
        for event in watcher:
            print(event.name, event.type)
            break`
  },
  'sandbox.pty.*': {
    e2b: `from e2b import PtySize, Sandbox

with Sandbox.create() as sandbox:
    terminal = sandbox.pty.create(PtySize(rows=24, cols=80))
    sandbox.pty.send_stdin(terminal.pid, b"echo hello from PTY\\n")
    sandbox.pty.resize(terminal.pid, PtySize(rows=30, cols=100))
    sandbox.pty.kill(terminal.pid)`,
    cube: `from cubesandbox import PtySize, Sandbox

with Sandbox.create() as sandbox:
    handle = sandbox.pty.create(PtySize(rows=24, cols=80))
    handle.send_stdin(b"echo hello from PTY\\n")
    handle.resize(PtySize(rows=30, cols=100))
    handle.kill()`
  },
  'sandbox.git.*': {
    e2b: `from e2b import Sandbox

with Sandbox.create() as sandbox:
    # The base sandbox-code template does not include Git.
    sandbox.commands.run(
        "apt-get update -qq && apt-get install -y -qq git",
        timeout=120,
    )
    sandbox.git.clone(
        "https://github.com/octocat/Hello-World.git",
        path="/tmp/hello-world",
        depth=1,
    )
    print(sandbox.git.status("/tmp/hello-world"))`,
    cube: `from cubesandbox import Sandbox

with Sandbox.create() as sandbox:
    # The base sandbox-code template does not include Git.
    sandbox.commands.run(
        "apt-get update -qq && apt-get install -y -qq git",
        timeout=120,
    )
    sandbox.commands.run(
        "git clone https://github.com/octocat/Hello-World.git /tmp/hello-world"
    )
    print(sandbox.commands.run("git -C /tmp/hello-world status").stdout)`
  },
  'sandbox.run_code()': {
    e2b: `from e2b_code_interpreter import Sandbox

# Install e2b-code-interpreter and use its matching version.
with Sandbox.create() as sandbox:
    execution = sandbox.run_code("x = 6 * 7; x")
    print(execution.results[0].text)`,
    cube: `from cubesandbox import Sandbox

with Sandbox.create() as sandbox:
    execution = sandbox.run_code("x = 6 * 7; x")
    print(execution.results[0].text)`
  },
  'create / list / delete_snapshot': {
    e2b: `from e2b import Sandbox

with Sandbox.create() as sandbox:
    sandbox.files.write("/tmp/state.txt", "snapshot me")
    snapshot = sandbox.create_snapshot(name="docs-example")

paginator = Sandbox.list_snapshots(name="docs-example")
while paginator.has_next:
    print([item.snapshot_id for item in paginator.next_items()])

Sandbox.delete_snapshot(snapshot.snapshot_id)`,
    cube: `from cubesandbox import Sandbox

with Sandbox.create() as sandbox:
    sandbox.files.write("/tmp/state.txt", "snapshot me")
    snapshot = sandbox.create_snapshot(name="docs-example")

snapshots, next_token = Sandbox.list_snapshots(name="docs-example")
print([item.snapshot_id for item in snapshots])

print(Sandbox.delete_snapshot(snapshot.snapshot_id))  # True on success.`
  },
  'sandbox.fork()': {
    e2b: `from e2b import Sandbox

with Sandbox.create() as parent:
    parent.files.write("/tmp/shared.txt", "before fork")
    children = parent.fork(count=2, timeout=60)
    for child in children:
        if isinstance(child, Exception):
            print("fork failed:", child)
            continue
        print(child.files.read("/tmp/shared.txt"))
        child.kill()`,
    cube: `from cubesandbox import Sandbox

with Sandbox.create() as parent:
    parent.files.write("/tmp/shared.txt", "before fork")
    children = parent.fork(count=2, timeout=60)
    for child in children:
        if isinstance(child, Exception):
            print("fork failed:", child)
            continue
        print(child.files.read("/tmp/shared.txt"))
        child.kill()`
  },
  'Volume.create / connect / list / destroy': {
    e2b: `from uuid import uuid4
from e2b import Volume

volume = Volume.create(f"docs-{uuid4().hex[:8]}")
connected = Volume.connect(volume.volume_id)
print(connected.volume_id)
print([(item.volume_id, item.name) for item in Volume.list()])
Volume.destroy(volume.volume_id)`,
    cube: `from uuid import uuid4
from cubesandbox import Volume

volume = Volume.create(f"docs-{uuid4().hex[:8]}")
connected = Volume.connect(volume.volume_id)
print(connected.volume_id)
print([(item.volume_id, item.name) for item in Volume.list()])
Volume.destroy(volume.volume_id)`
  },
  'volume.read_file / write_file / list': {
    e2b: `from uuid import uuid4
from e2b import Volume

volume = Volume.create(f"docs-{uuid4().hex[:8]}")
try:
    volume.write_file("/hello.txt", "hello", force=True)
    print(volume.read_file("/hello.txt"))
    print([entry.name for entry in volume.list("/")])
finally:
    Volume.destroy(volume.volume_id)`,
    cube: `from cubesandbox import Sandbox, Volume

volume = Volume.create("docs-data")
try:
    # Mount the volume, then use sandbox.files to reach its contents.
    with Sandbox.create(volume_mounts={"/data": volume}) as sandbox:
        sandbox.files.write("/data/hello.txt", "hello")
        print(sandbox.files.read("/data/hello.txt"))
finally:
    Volume.destroy(volume.volume_id)`
  },
  'Template.build() fluent API': {
    e2b: `from uuid import uuid4
from e2b import Template

template = (
    Template()
    .from_python_image("3.12")
    .run_cmd("python --version")
)
# Building can take several minutes and creates a project template.
Template.build(template, f"docs-python-{uuid4().hex[:8]}")`,
    cube: `from cubesandbox import Template

# Cube builds templates from an OCI image and probes readiness.
build = Template.build(
    name="docs-python",
    image="python:3.12-slim",
    start_cmd="python -m http.server 80",
    exposed_ports=[80],
)
print(build.job_id)`
  },
  'sandbox.get_mcp_token()': {
    e2b: `from e2b import Sandbox

mcp = {
    "github/modelcontextprotocol/servers": {
        "run_cmd": "npx -y @modelcontextprotocol/server-everything"
    }
}
with Sandbox.create(mcp=mcp) as sandbox:
    print(sandbox.get_mcp_token())`,
    cube: `from cubesandbox import Sandbox

# The mcp-gateway template ships the reference gateway program.
with Sandbox.create(mcp={"time": {}}) as sandbox:
    print(sandbox.get_mcp_url())
    print(sandbox.get_mcp_token())`
  },
  'Sandbox.create(iam=...)': {
    e2b: `from e2b import Sandbox, Secret

identity = Secret.iam_token(
    audience="sts.amazonaws.com",
    token_type="JWT-SVID",
)
with Sandbox.create(iam={"tokens": {"aws": identity}}) as sandbox:
    print(sandbox.sandbox_id)`,
    cube: null
  },
  'Secret.*': {
    e2b: `from uuid import uuid4
from e2b import Secret

name = f"docs-{uuid4().hex[:8]}"
created = Secret.create(name, "initial-value")
Secret.update(name, "rotated-value")
print(Secret.get_info(name))  # Values are write-only.
print(Secret.list(limit=10).next_items())
Secret.destroy(created.secret_id)`,
    cube: null
  },
  'AsyncSandbox / async resources': {
    e2b: `import asyncio
from e2b import AsyncSandbox

async def main():
    sandbox = await AsyncSandbox.create()
    try:
        result = await sandbox.commands.run("echo async")
        print(result.stdout)
    finally:
        await sandbox.kill()

asyncio.run(main())`,
    cube: null
  },
  'sandbox.rollback()': {
    e2b: null,
    cube: `from cubesandbox import Sandbox

with Sandbox.create() as sandbox:
    sandbox.files.write("/tmp/value.txt", "v1")
    snapshot = sandbox.create_snapshot()
    try:
        sandbox.files.write("/tmp/value.txt", "v2")
        sandbox.rollback(snapshot.snapshot_id)
        print(sandbox.files.read("/tmp/value.txt"))
    finally:
        Sandbox.delete_snapshot(snapshot.snapshot_id)`
  },
  'Sandbox.create(template=snapshot_id)': {
    e2b: `from e2b import Sandbox

with Sandbox.create() as source:
    source.files.write("/tmp/state.txt", "portable state")
    snapshot = source.create_snapshot()

try:
    # E2B snapshots act as templates; any node can restore them.
    with Sandbox.create(template=snapshot.snapshot_id) as restored:
        print(restored.files.read("/tmp/state.txt"))
finally:
    Sandbox.delete_snapshot(snapshot.snapshot_id)`,
    cube: `from cubesandbox import Sandbox

with Sandbox.create() as source:
    source.files.write("/tmp/state.txt", "portable state")
    snapshot = source.create_snapshot()

try:
    # Cross-node placement requires an S3-backed, remotely ready snapshot.
    with Sandbox.create(template=snapshot.snapshot_id) as restored:
        print(restored.files.read("/tmp/state.txt"))
finally:
    Sandbox.delete_snapshot(snapshot.snapshot_id)`
  },
  'Volume.create(driver=...) / VolumeMount(read_only=True)': {
    e2b: null,
    cube: `from cubesandbox import Sandbox, Volume, VolumeMount

# CUBE_VOLUME_DRIVER must name a configured Cube volume plugin.
volume = Volume.create("docs-data", driver=os.environ["CUBE_VOLUME_DRIVER"])
try:
    with Sandbox.create(
        volume_mounts={"/data": VolumeMount(volume, read_only=True)}
    ) as sandbox:
        print(sandbox.files.list("/data"))
finally:
    Volume.destroy(volume.volume_id)`
  },
  'sandbox.update_network(rules=...)': {
    e2b: null,
    cube: `from cubesandbox import Action, Match, Rule, Sandbox

rule = Rule(
    name="allow-example-api",
    match=Match(host="api.example.com", path="/v1/"),
    action=Action(allow=True, audit="metadata"),
)
with Sandbox.create() as sandbox:
    sandbox.update_network({"rules": [rule]})`
  },
  'Sandbox.create(distribution_scope=...)': {
    e2b: null,
    cube: `from cubesandbox import Sandbox

# Use a compute-node ID or host IP reported by CubeOps.
with Sandbox.create(
    distribution_scope=[os.environ["CUBE_COMPUTE_NODE"]]
) as sandbox:
    print(sandbox.sandbox_id)`
  },
  'GET /v1/metrics/resource': {
    e2b: null,
    cube: `import os
from urllib.request import urlopen

cubelet_url = os.environ.get("CUBELET_URL", "http://127.0.0.1:9998")
url = f'{cubelet_url.rstrip("/")}/v1/metrics/resource'
with urlopen(url, timeout=5) as response:
    print(response.read().decode())`
  }
}

const zhComments = {
  '# The context manager kills the sandbox when the block exits.': '# 上下文管理器会在代码块结束时自动销毁沙箱。',
  '# Preserve state for a later resume.': '# 保存当前状态，之后可以继续恢复运行。',
  '# Replace the current timeout with five minutes.': '# 把当前生命周期超时时间重新设置为 5 分钟。',
  '# Omitted fields are cleared because this replaces the policy.': '# 此操作会整体替换策略，因此未填写的字段会被清空。',
  '# The process keeps running in the sandbox.': '# 断开句柄后，进程仍会继续在沙箱中运行。',
  '# Install e2b-code-interpreter and use its matching version.': '# 需要安装 e2b-code-interpreter，并与 e2b SDK 版本保持匹配。',
  '# Building can take several minutes and creates a project template.': '# 构建可能需要几分钟，并会在当前项目中创建模板。',
  '# Values are write-only.': '# 密钥值只可写入，不会由查询接口返回。',
  '# Cross-node placement requires an S3-backed, remotely ready snapshot.': '# 跨节点调度要求快照使用 S3 后端，并且远端状态已经就绪。',
  '# CUBE_VOLUME_DRIVER must name a configured Cube volume plugin.': '# CUBE_VOLUME_DRIVER 必须指向已经配置的 Cube Volume 插件。',
  '# Use a compute-node ID or host IP reported by CubeOps.': '# 使用 CubeOps 返回的计算节点 ID 或宿主机 IP。',
  '# The base sandbox-code template does not include Git.': '# 基础 sandbox-code 模板默认不包含 Git。',
  '# The mcp-gateway template ships the reference gateway program.': '# mcp-gateway 模板自带参考网关程序。',
  '# E2B snapshots act as templates; any node can restore them.': '# E2B 的快照本身就是模板，任何节点都可以恢复。',
  '# CUBE_TEMPLATE_ID is picked up automatically when template is omitted.': '# 省略 template 时会自动读取 CUBE_TEMPLATE_ID。',
  '# list_v2() follows the cursor and returns every matching sandbox.': '# list_v2() 会自动跟随游标，返回所有匹配的沙箱。',
  '# Raises not-found if the ID no longer exists.': '# ID 不存在时会抛出 not-found 错误。',
  '# Returns a plain dict.': '# 返回普通 dict。',
  '# True on success.': '# 删除成功返回 True。',
  '# Mount the volume, then use sandbox.files to reach its contents.': '# 先挂载 Volume，再通过 sandbox.files 访问其中的内容。',
  '# Cube builds templates from an OCI image and probes readiness.': '# Cube 从 OCI 镜像构建模板，并自动探测就绪状态。'
}

const pythonKeywords = new Set(['and', 'as', 'async', 'await', 'break', 'class', 'continue', 'def', 'del', 'elif', 'else', 'except', 'finally', 'for', 'from', 'global', 'if', 'import', 'in', 'is', 'lambda', 'nonlocal', 'not', 'or', 'pass', 'raise', 'return', 'try', 'while', 'with', 'yield'])
const pythonBuiltins = new Set(['Exception', 'False', 'None', 'True', 'bytes', 'dict', 'enumerate', 'f', 'int', 'isinstance', 'len', 'list', 'print', 'range', 'str', 'sum'])

function localizedExample(api, kind) {
  let source = examples[api]?.[kind] || ''
  if (kind === 'e2b' && /\b(?:Async)?Sandbox\.create\(/.test(source)) {
    // The official E2B SDK never reads CUBE_TEMPLATE_ID by itself.
    source = `import os\n${source}`
    source = source.replace(/\b(AsyncSandbox|Sandbox)\.create\(\)/g, '$1.create(template=os.environ["CUBE_TEMPLATE_ID"])')
    source = source.replace(/\b(AsyncSandbox|Sandbox)\.create\((?!template=)/g, '$1.create(template=os.environ["CUBE_TEMPLATE_ID"], ')
  }
  if (props.locale === 'zh') {
    for (const [english, chinese] of Object.entries(zhComments)) source = source.replaceAll(english, chinese)
  }
  return source
}

function escapeHtml(value) {
  return value.replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;').replaceAll('"', '&quot;').replaceAll("'", '&#39;')
}

function highlightPython(source) {
  let html = ''
  let index = 0
  while (index < source.length) {
    const char = source[index]
    if (char === '#') {
      const end = source.indexOf('\n', index)
      const stop = end === -1 ? source.length : end
      html += `<span class="py-comment">${escapeHtml(source.slice(index, stop))}</span>`
      index = stop
      continue
    }
    if (char === '"' || char === "'") {
      const quote = char
      let end = index + 1
      while (end < source.length) {
        if (source[end] === '\\') end += 2
        else if (source[end++] === quote) break
      }
      html += `<span class="py-string">${escapeHtml(source.slice(index, end))}</span>`
      index = end
      continue
    }
    if (/[A-Za-z_]/.test(char)) {
      let end = index + 1
      while (end < source.length && /[A-Za-z0-9_]/.test(source[end])) end++
      const word = source.slice(index, end)
      let tokenClass = ''
      if (pythonKeywords.has(word)) tokenClass = 'py-keyword'
      else if (pythonBuiltins.has(word)) tokenClass = 'py-builtin'
      else if (/^\s*\(/.test(source.slice(end))) tokenClass = 'py-function'
      html += tokenClass ? `<span class="${tokenClass}">${word}</span>` : word
      index = end
      continue
    }
    if (/\d/.test(char)) {
      let end = index + 1
      while (end < source.length && /[\d._]/.test(source[end])) end++
      html += `<span class="py-number">${escapeHtml(source.slice(index, end))}</span>`
      index = end
      continue
    }
    if (/[=+*:\-]/.test(char)) html += `<span class="py-operator">${escapeHtml(char)}</span>`
    else html += escapeHtml(char)
    index++
  }
  return html
}

function highlightShell(source) {
  return source.split('\n').map((line) => {
    const install = line.match(/^(pip)( install)(.*)$/)
    if (install) {
      return `<span class="sh-command">${install[1]}</span><span class="sh-subcommand">${install[2]}</span>${escapeHtml(install[3])}`
    }
    const env = line.match(/^(export) ([A-Z0-9_]+)(=)(.*)$/)
    if (env) {
      return `<span class="sh-keyword">${env[1]}</span> <span class="sh-variable">${env[2]}</span><span class="sh-operator">${env[3]}</span><span class="sh-string">${env[4]}</span>`
    }
    return escapeHtml(line)
  }).join('\n')
}

function apiLabels(api) {
  return api.split(/\s+\/\s+/)
}

const activeCategory = ref('all')
const query = ref('')
const copiedApi = ref('')
const exampleTab = ref({})
let copyResetTimer
const t = computed(() => copy[props.locale] || copy.en)
const categories = ['lifecycle', 'runtime', 'storage', 'platform', 'extension']
const filteredRows = computed(() => {
  const needle = query.value.trim().toLowerCase()
  return rows.filter((row) => (activeCategory.value === 'all' || row.category === activeCategory.value) && (!needle || `${row.api} ${row.cube} ${row.en} ${row.zh} ${purposes[row.api]?.en} ${purposes[row.api]?.zh}`.toLowerCase().includes(needle)))
})
const groupedRows = computed(() => categories.map((category) => ({ category, rows: filteredRows.value.filter((row) => row.category === category) })).filter((group) => group.rows.length))
const counts = computed(() => Object.keys(t.value.statuses).map((status) => ({ status, count: rows.filter((row) => row.status === status).length })))
const versionCounts = computed(() => ({
  total: sdkVersions.length,
  pass: sdkVersions.filter(([, , verdict]) => verdict === 'ok').length,
  partial: sdkVersions.filter(([, , verdict]) => verdict === 'partial').length,
  fail: sdkVersions.filter(([, , verdict]) => verdict === 'no').length
}))

async function copyExample(api) {
  await copyCode(api, localizedExample(api, tabOf(api)))
}

function hasTab(api, kind) {
  return Boolean(examples[api] && examples[api][kind])
}

function tabOf(api) {
  return exampleTab.value[api] || (examples[api] && examples[api].e2b ? 'e2b' : 'cube')
}

function setTab(api, kind) {
  exampleTab.value[api] = kind
}

async function copyCode(key, code) {
  try {
    await navigator.clipboard.writeText(code)
  } catch {
    const textarea = document.createElement('textarea')
    textarea.value = code
    textarea.style.position = 'fixed'
    textarea.style.opacity = '0'
    document.body.appendChild(textarea)
    textarea.select()
    document.execCommand('copy')
    textarea.remove()
  }
  copiedApi.value = key
  window.clearTimeout(copyResetTimer)
  copyResetTimer = window.setTimeout(() => { copiedApi.value = '' }, 1600)
}
</script>

<template>
  <div class="e2b-page" :data-lang="locale">
    <div class="e2b-wrap">
      <section class="e2b-hero">
        <p class="e2b-label e2b-accent">{{ t.heroLabel }}</p>
        <h1>E2B <em>×</em> CubeSandbox</h1>
        <p class="e2b-lead">{{ t.lead }}</p>
        <p class="e2b-sub">{{ t.intro }}</p>
        <div class="e2b-hero-meta">
          <span>{{ t.heroMeta.result }} <b>{{ locale === 'zh' ? '全部通过' : 'All passed' }}</b></span>
          <span>{{ t.heroMeta.releases }} <b>1.0.0 – 2.62.0</b></span>
          <span>{{ t.heroMeta.latest }} <b>2.62.0</b></span>
          <span>{{ t.heroMeta.recommended }} <b>≥ 2.15.0</b></span>
        </div>
      </section>

      <section class="e2b-quickstart">
        <h2>{{ t.quickstartTitle }}</h2>
        <p>{{ t.quickstartIntro }}</p>
        <pre class="e2b-shell"><code v-html="highlightShell(setupCommands)"></code></pre>
      </section>

      <section class="e2b-figures">
        <div v-for="item in counts" :key="item.status" class="e2b-figure" :class="`is-${item.status}`">
          <small><i/>{{ t.statuses[item.status] }}</small>
          <b>{{ item.count }}</b>
        </div>
      </section>

      <section class="e2b-section" id="version-compatibility">
        <div class="e2b-section-head">
          <div>
            <p class="e2b-label e2b-accent">{{ t.versionLabel }}</p>
            <h2>{{ t.versionTitle }}</h2>
          </div>
          <div>
            <p>{{ t.versionLead }}</p>
            <div class="e2b-legend">
              <span class="pass"><i/>{{ t.legend.pass }} · {{ versionCounts.pass }}</span>
              <span class="partial"><i/>{{ t.legend.partial }} · {{ versionCounts.partial }}</span>
              <span class="fail"><i/>{{ t.legend.fail }} · {{ versionCounts.fail }}</span>
            </div>
          </div>
        </div>
        <div class="e2b-wall">
          <div v-for="[version, passed, verdict] in sdkVersions" :key="version" class="e2b-block" :class="WALL_TONE[verdict]">
            <span class="e2b-tip"><b>{{ version }}</b> · {{ t.bandVerdicts[verdict === 'ok' ? 'ok' : verdict === 'partial' ? 'warn' : 'bad'] }}</span>
          </div>
        </div>
        <div class="e2b-axis">
          <span>1.0.0</span>
          <span>2.62.0</span>
        </div>

        <div class="e2b-bands">
          <div class="e2b-band e2b-head">
            <span class="range">{{ t.bandCols[0] }}</span>
            <span class="e2b-chip">{{ t.bandCols[1] }}</span>
            <span class="num">{{ t.bandCols[2] }}</span>
            <span class="count">{{ t.bandCols[3] }}</span>
            <span class="why">{{ t.bandCols[4] }}</span>
          </div>
          <div v-for="band in t.bands" :key="band.range" class="e2b-band">
            <span class="range">{{ band.range }}</span>
            <span class="e2b-chip" :class="band.verdict">{{ t.bandVerdicts[band.verdict] }}</span>
            <span class="num">{{ band.suite }}</span>
            <span class="count">{{ band.count }}</span>
            <p class="why">{{ band.why }}</p>
          </div>
        </div>

        <ol class="e2b-notes">
          <li v-for="note in t.versionNotes" :key="note" class="e2b-foot-note">{{ note }}</li>
        </ol>
      </section>

      <section class="e2b-section" id="sdk-api-matrix">
        <div class="e2b-section-head">
          <div>
            <p class="e2b-label e2b-accent">{{ t.matrixLabel }}</p>
            <h2>{{ t.matrixTitle }}</h2>
          </div>
          <div>
            <p>{{ t.matrixIntro }}</p>
            <p class="e2b-warn-note">{{ t.matrixVersionWarning }}</p>
            <p class="e2b-note">{{ t.exampleNote }}</p>
          </div>
        </div>

        <div class="e2b-tools">
          <div class="e2b-filter">
            <button type="button" :aria-pressed="activeCategory === 'all'" @click="activeCategory='all'">{{ t.all }}</button>
            <button v-for="category in categories" :key="category" type="button" :aria-pressed="activeCategory === category" @click="activeCategory=category">{{ t.categories[category] }}</button>
          </div>
          <label class="e2b-search"><span>⌕</span><input v-model="query" type="search" :placeholder="t.search"></label>
        </div>

        <div v-if="groupedRows.length">
          <div v-for="group in groupedRows" :key="group.category" class="e2b-group">
            <div class="e2b-group-head">
              <h3>{{ t.categories[group.category] }}</h3>
              <span>{{ group.rows.length }}</span>
            </div>
            <div class="e2b-rows">
              <div v-for="row in group.rows" :key="row.api" class="e2b-row">
                <div class="e2b-api"><code v-for="label in apiLabels(row.api)" :key="label">{{ label }}</code></div>
                <div class="e2b-purpose">{{ purposes[row.api]?.[locale] || purposes[row.api]?.en }}</div>
                <div><span class="e2b-chip" :class="{ compatible: 'ok', partial: 'warn', mapped: 'alt', missing: 'bad', extension: 'ext' }[row.status]">{{ t.statuses[row.status] }}</span></div>
                <div class="e2b-detail">{{ row[locale] || row.en }}</div>
                <details class="e2b-example">
                  <summary><span class="show-example">{{ t.example }} ↓</span><span class="hide-example">{{ t.hideExample }} ↑</span></summary>
                  <div class="e2b-tabs">
                    <button v-if="hasTab(row.api, 'e2b')" type="button" :class="{ active: tabOf(row.api) === 'e2b' }" @click="setTab(row.api, 'e2b')">E2B SDK</button>
                    <button v-if="hasTab(row.api, 'cube')" type="button" :class="{ active: tabOf(row.api) === 'cube' }" @click="setTab(row.api, 'cube')">CubeSandbox SDK</button>
                  </div>
                  <div class="e2b-code">
                    <button type="button" class="e2b-copy" :class="{ copied: copiedApi === row.api }" @click="copyExample(row.api)">{{ copiedApi === row.api ? t.copiedCode : t.copyCode }}</button>
                    <pre><code v-html="highlightPython(localizedExample(row.api, tabOf(row.api)))"></code></pre>
                  </div>
                </details>
              </div>
            </div>
          </div>
        </div>
        <div v-else class="e2b-empty">{{ t.noResults }}</div>
      </section>

      <section class="e2b-section" id="cube-api">
        <div class="e2b-section-head">
          <div>
            <p class="e2b-label e2b-accent">{{ t.apiLabel }}</p>
            <h2>{{ t.apiTitle }}</h2>
          </div>
          <div>
            <p>{{ t.apiIntro }}</p>
          </div>
        </div>
        <div v-for="group in cubeApi" :key="group.key" class="e2b-group">
          <div class="e2b-group-head">
            <h3>{{ t.apiGroups[group.key] }}</h3>
            <span>{{ group.endpoints.length }}</span>
          </div>
          <div class="e2b-rows">
            <div v-for="ep in group.endpoints" :key="ep.path" class="e2b-api-row">
              <code class="e2b-ep"><b>{{ ep.m }}</b>{{ ep.path }}</code>
              <span class="e2b-chip" :class="ep.status === 'missing' ? 'bad' : ep.status">{{ t.apiStatuses[ep.status] }}</span>
              <p class="e2b-ep-note">{{ ep[locale] || ep.en }}</p>
            </div>
          </div>
          <p v-if="group.key === 'envd'" class="e2b-note">{{ t.envdNote }}</p>
        </div>
      </section>

      <footer class="e2b-foot">
        <div class="e2b-foot-links">
          <a href="https://github.com/TencentCloud/CubeSandbox/blob/master/openapi.yml">Cube OpenAPI ↗</a>
          <a href="https://github.com/e2b-dev/E2B/blob/main/spec/openapi.yml">E2B OpenAPI ↗</a>
          <a href="https://github.com/e2b-dev/E2B/blob/main/packages/python-sdk/e2b/sandbox_sync/main.py">E2B Python SDK ↗</a>
          <a href="https://github.com/TencentCloud/CubeSandbox/blob/master/tests/e2e/sdk_compat/e2b-versions.txt">Validated versions ↗</a>
          <a href="https://github.com/TencentCloud/CubeSandbox/issues/1819">Compatibility tracker ↗</a>
        </div>
        <span>{{ t.sourceIntro }}</span>
      </footer>
    </div>
  </div>
</template>
