<script setup>
import { computed, ref } from 'vue'
import cubeLogo from '../../assets/cube-sandbox-logo.png'

const props = defineProps({ locale: { type: String, default: 'en' } })

const copy = {
  en: {
    eyebrow: 'SDK COMPATIBILITY MATRIX', title: 'E2B × CubeSandbox',
    subtitle: 'Same workflow, not identical behavior.',
    intro: 'Compare public SDK methods by name, see what works unchanged, and find the Cube alternative when behavior differs.',
    quickstartTitle: 'Python SDK quick start',
    quickstartIntro: 'Use the E2B Python SDK for existing E2B code, or the native CubeSandbox Python SDK for Cube APIs. Install and configure them as follows:',
    matrixTitle: 'SDK API matrix', matrixIntro: 'Compatibility is assessed at the public method level. A matching name does not imply matching defaults, return types, or side effects.', matrixVersionWarning: 'Compatibility notice: Pin a validated E2B Python SDK (2.21.0, 2.26.0, or 2.29.5). e2b 2.38 and later break e2b-code-interpreter 2.9.0; the last interpreter-compatible core is 2.37.1 and is not yet validated against Cube.',
    search: 'Search an API, such as connect or files.write', all: 'All APIs', api: 'SDK API', purpose: 'What it does', status: 'Status', difference: 'Behavior and differences', noResults: 'No APIs match this filter.',
    example: 'View example', hideExample: 'Hide example', copyCode: 'Copy', copiedCode: 'Copied', exampleNote: 'Examples use the validated E2B Python SDK 2.29.5 against Cube. Set E2B_API_KEY and CUBE_TEMPLATE_ID first. Compatible, partial, and mapped examples were verified against Cube; unavailable examples show calls that Cube does not implement and are expected to fail. For run_code, install a matching e2b-code-interpreter — 2.8.1 pairs with 2.26.0; 2.38+ cores break 2.9.0.',
    timeoutTitle: 'The timeout difference that matters most', timeoutIntro: 'Both Python APIs use seconds, but the lifecycle clocks are not equivalent.',
    e2bTimeoutTitle: 'E2B: deadline / TTL', e2bTimeoutText: 'connect(id, timeout=60) extends the deadline only when the new deadline is later. It never shortens a longer remaining lifetime.',
    cubeTimeoutTitle: 'Cube: sliding idle window', cubeTimeoutText: 'connect(id, timeout=60) only extends the idle window when the requested value is longer, whether the sandbox is running or must first resume. Use set_timeout(60) to replace the current window, including shortening it.',
    timeoutFootnote: 'Cube also supports -1 (NEVER_TIMEOUT); current E2B v2 create/connect reject negative values.',
    sourceTitle: 'Pinned sources', sourceIntro: 'The matrix is based on implementation and machine-readable API definitions, not product-name similarity.',
    statuses: { compatible: 'Compatible', partial: 'Partial', mapped: 'Alternative', missing: 'Not available', extension: 'Cube extension' },
    categories: { lifecycle: 'Sandbox lifecycle', runtime: 'Commands, files & PTY', storage: 'Snapshots, volumes & templates', platform: 'Platform services', extension: 'Cube extensions' }
  },
  zh: {
    eyebrow: 'SDK 兼容性矩阵', title: 'E2B × CubeSandbox',
    subtitle: '工作流相似，行为并非完全相同。',
    intro: '按公开 SDK 方法逐项对比：哪些可以原样使用、哪些只是部分兼容，以及行为不同时应使用哪个 Cube 接口。',
    quickstartTitle: 'Python SDK 快速上手',
    quickstartIntro: '已有 E2B 代码可以使用 E2B Python SDK，也可以使用 CubeSandbox 原生 Python SDK。安装并配置以下环境变量即可：',
    matrixTitle: 'SDK API 对比矩阵', matrixIntro: '这里按公开方法判断兼容性。同名方法不代表默认值、返回类型和副作用完全一致。', matrixVersionWarning: '兼容性提示：请钉扎已验证的 E2B Python SDK（2.21.0、2.26.0 或 2.29.5）。e2b 2.38 及以上与 e2b-code-interpreter 2.9.0 不兼容；当前解释器可配对的最高核心是 2.37.1，且尚未针对 Cube 验证。',
    search: '搜索 API，例如 connect 或 files.write', all: '全部 API', api: 'SDK API', purpose: 'API 作用', status: '兼容状态', difference: '行为与差异说明', noResults: '没有符合当前条件的 API。',
    example: '查看示例', hideExample: '收起示例', copyCode: '复制', copiedCode: '已复制', exampleNote: '示例使用已验证的 E2B Python SDK 2.29.5 连接 Cube。运行前请设置 E2B_API_KEY 和 CUBE_TEMPLATE_ID。兼容、部分兼容和映射条目均已在 Cube 上实测；暂不支持条目用于展示 Cube 尚未实现的调用，执行时会按预期失败。run_code 需要匹配的 e2b-code-interpreter：2.8.1 可与 2.26.0 配对，2.38 及以上核心会破坏 2.9.0。',
    timeoutTitle: '最需要注意的 timeout 差异', timeoutIntro: '双方 Python API 都使用秒，但控制的生命周期时钟并不相同。',
    e2bTimeoutTitle: 'E2B：固定 deadline / TTL', e2bTimeoutText: 'connect(id, timeout=60) 只有在新期限更晚时才延长，不会缩短原本更长的剩余生命周期。',
    cubeTimeoutTitle: 'Cube：滑动空闲窗口', cubeTimeoutText: '无论沙箱已在运行还是需要先恢复，connect(id, timeout=60) 都只会在请求窗口更长时延长空闲期限。若要覆盖（包括缩短）当前窗口，应调用 set_timeout(60)。',
    timeoutFootnote: 'Cube 还支持 -1（NEVER_TIMEOUT）；当前 E2B v2 create/connect 不接受负数。',
    sourceTitle: '固定版本的依据', sourceIntro: '本矩阵依据双方实现和机器可读 API 定义，而不是仅根据产品名称或同名方法判断。',
    statuses: { compatible: '兼容', partial: '部分兼容', mapped: '有替代方案', missing: '暂不支持', extension: 'Cube 扩展' },
    categories: { lifecycle: '沙箱生命周期', runtime: '命令、文件与 PTY', storage: '快照、Volume 与模板', platform: '平台服务', extension: 'Cube 扩展能力' }
  }
}

const setupCommands = `pip install "e2b==2.29.5"
pip install cubesandbox

export CUBE_API_URL="http://127.0.0.1:3000"
export CUBE_API_KEY="e2b_000000"
export CUBE_TEMPLATE_ID="tpl-xxx"
export E2B_API_URL="http://127.0.0.1:3000"
export E2B_API_KEY="e2b_000000"`

const rows = [
  { category: 'lifecycle', api: 'Sandbox.create()', status: 'partial', cube: 'Sandbox.create()', en: 'Works through Cube\'s v1 create route. Pass CUBE_TEMPLATE_ID explicitly: the E2B SDK does not read that variable and otherwise requests its default base template.', zh: '可以通过 Cube 的 v1 创建路由工作。需要显式传入 CUBE_TEMPLATE_ID：E2B SDK 不会自动读取该变量，否则会请求其默认的 base 模板。' },
  { category: 'lifecycle', api: 'Sandbox.connect()', status: 'partial', cube: 'Sandbox.connect(id)', en: 'Works through Cube\'s v1 connect route. A positive timeout only extends the remaining idle window when the requested value is longer; it never shortens a running or paused sandbox. Use set_timeout() to replace the window. Cube also accepts -1 (NEVER_TIMEOUT); E2B v2 create/connect reject negatives.', zh: '可以通过 Cube 的 v1 connect 路由工作。正数 timeout 只会在请求窗口更长时延长剩余空闲时间，不会缩短运行中或已暂停沙箱的期限。若要覆盖当前窗口，应调用 set_timeout()。Cube 还接受 -1（NEVER_TIMEOUT）；E2B v2 的 create/connect 不接受负数。' },
  { category: 'lifecycle', api: 'Sandbox.list()', status: 'partial', cube: 'Sandbox.list_v2()', en: 'Both have a v2 list route. E2B returns a paginator and supports cursor, order, start-time, and template filters; Cube returns a list and supports metadata/state/limit only.', zh: '双方都有 v2 查询路由。E2B 返回 paginator，并支持游标、排序、开始时间和模板筛选；Cube 返回 list，目前仅支持 metadata/state/limit。' },
  { category: 'lifecycle', api: 'sandbox.get_info()', status: 'compatible', cube: 'sandbox.get_info()', en: 'Core identity, template, state, timestamps, metadata, CPU, and memory fields align. Cube additionally exposes disk size, CPU millicores, and transient states.', zh: '核心身份、模板、状态、时间、metadata、CPU 和内存字段一致。Cube 额外提供磁盘大小、CPU millicore 和瞬时状态。' },
  { category: 'lifecycle', api: 'sandbox.is_running()', status: 'mapped', cube: 'sandbox.get_info().state', en: 'Cube Python has no is_running method. Read get_info().state and compare it with running.', zh: 'Cube Python 没有 is_running 方法，可读取 get_info().state 并与 running 比较。' },
  { category: 'lifecycle', api: 'sandbox.kill()', status: 'partial', cube: 'sandbox.kill()', en: 'Both destroy the sandbox. E2B static helpers return False for a missing ID; Cube raises not-found and returns no value on success.', zh: '双方都会销毁沙箱。E2B 静态方法在 ID 不存在时返回 False；Cube 抛出 not-found，成功时无返回值。' },
  { category: 'lifecycle', api: 'sandbox.pause()', status: 'partial', cube: 'sandbox.pause()', en: 'Both preserve memory. E2B supports keep_memory=False for filesystem-only pause; Cube always snapshots memory. Cube wait options control client polling.', zh: '双方都能保留内存。E2B 支持 keep_memory=False 的仅文件系统暂停；Cube 总是保存内存。Cube 的 wait 参数只控制客户端轮询。' },
  { category: 'lifecycle', api: 'sandbox.set_timeout()', status: 'partial', cube: 'sandbox.set_timeout()', en: 'Both replace the current window. Cube treats it as an idle window and adds -1 (NEVER_TIMEOUT); E2B uses an expiration deadline and rejects negative values.', zh: '双方都会覆盖当前窗口。Cube 将其视为空闲窗口并增加 -1（NEVER_TIMEOUT）；E2B 使用到期 deadline，不接受负数。' },
  { category: 'lifecycle', api: 'sandbox.update_network()', status: 'partial', cube: 'sandbox.update_network()', en: 'Both replace the policy. Cube lacks egress_proxy, https_ports, and IAM transforms; it adds ordered L7 actions and established-flow revocation.', zh: '双方都以整包方式替换策略。Cube 不支持 egress_proxy、https_ports 或 IAM transform；但增加有序 L7 动作和已有连接撤销。' },
  { category: 'lifecycle', api: 'sandbox.get_metrics()', status: 'missing', cube: 'Cubelet Prometheus metrics', en: 'Cube does not implement E2B /sandboxes/{id}/metrics. It exports host-MicroVM and guest-workload metrics through Cubelet.', zh: 'Cube 未实现 E2B /sandboxes/{id}/metrics，而是通过 Cubelet 导出宿主机 MicroVM 与 Guest 工作负载指标。' },
  { category: 'runtime', api: 'sandbox.commands.run()', status: 'compatible', cube: 'sandbox.commands.run()', en: 'Command, cwd, environment, user, stdout/stderr, exit code, and execution timeout work through the compatible envd process protocol.', zh: '命令、cwd、环境变量、用户、stdout/stderr、退出码和执行超时通过兼容的 envd process 协议工作。' },
  { category: 'runtime', api: 'commands.connect()', status: 'partial', cube: 'sandbox.pty.connect()', en: 'The envd protocol supports compatible E2B handles, but Cube Python Commands exposes run only. Native Cube reconnection is available for PTYs.', zh: 'envd 协议可供兼容的 E2B 句柄使用，但 Cube Python Commands 只暴露 run；Cube 原生重连能力目前用于 PTY。' },
  { category: 'runtime', api: 'commands.list / kill / send_stdin', status: 'missing', cube: 'sandbox.pty.*', en: 'Not exposed on Cube Python Commands. Use PTY control for interactive processes or operating-system tools through commands.run.', zh: 'Cube Python Commands 未暴露这些方法。交互进程可使用 PTY 控制，其他场景可通过 commands.run 执行系统工具。' },
  { category: 'runtime', api: 'files.read / write / write_files', status: 'compatible', cube: 'sandbox.files.*', en: 'Common text/binary transfer and batch-write shapes are supported over the compatible filesystem protocol.', zh: '常用文本/二进制传输和批量写入通过兼容的文件系统协议提供。' },
  { category: 'runtime', api: 'files.list / exists / remove / rename / make_dir', status: 'compatible', cube: 'sandbox.files.*', en: 'The common directory and file-management workflow is aligned and covered by dual-backend tests.', zh: '常用目录和文件管理工作流已经对齐，并由双后端测试覆盖。' },
  { category: 'runtime', api: 'files.get_info()', status: 'mapped', cube: 'files.stat()', en: 'The capability exists under a different Cube Python name and returns a dict rather than E2B\'s typed object.', zh: 'Cube Python 以不同的方法名提供该能力，返回 dict，而不是 E2B 的类型化对象。' },
  { category: 'runtime', api: 'files.watch_dir()', status: 'partial', cube: 'files.watch_dir()', en: 'Both stream directory changes. Handle lifecycle, event types, and callback/polling APIs differ.', zh: '双方都能流式监听目录变化，但句柄生命周期、事件类型以及回调/轮询 API 不同。' },
  { category: 'runtime', api: 'sandbox.pty.*', status: 'compatible', cube: 'sandbox.pty.*', en: 'Create, connect, resize, input, kill, disconnect, and wait are available. Callback and exception types remain SDK-specific.', zh: '支持 create、connect、resize、输入、kill、disconnect 和 wait；回调与异常类型仍由各 SDK 自行定义。' },
  { category: 'runtime', api: 'sandbox.git.*', status: 'mapped', cube: 'commands.run("git …")', en: 'Cube Python has no high-level Git facade. Git installed in the template can be driven through the command API.', zh: 'Cube Python 没有高层 Git facade；如果模板中安装了 Git，可以通过命令 API 调用。' },
  { category: 'runtime', api: 'sandbox.run_code()', status: 'partial', cube: 'sandbox.run_code()', en: 'Stateful execution and rich results work when the template includes code interpreter. Compatibility depends on SDK and template/envd versions.', zh: '模板包含代码解释器时支持有状态执行和富媒体结果。兼容性同时取决于 SDK 与模板/envd 版本。' },
  { category: 'storage', api: 'create / list / delete_snapshot', status: 'partial', cube: 'Sandbox.*_snapshot()', en: 'Create and delete align. Cube list_snapshots returns a (list, next_token) tuple and has no name filter; E2B returns a paginator and accepts name=.', zh: '创建和删除工作流一致。Cube 的 list_snapshots 返回 (list, next_token) 二元组，且没有 name 过滤；E2B 返回 paginator，并支持 name=。' },
  { category: 'storage', api: 'sandbox.fork()', status: 'mapped', cube: 'sandbox.clone()', en: 'E2B reports each child independently. Cube composes snapshot + create and cleans up successful siblings after any creation failure.', zh: 'E2B 分别报告每个子实例；Cube 组合 snapshot + create，任一创建失败时清理已成功的兄弟实例。' },
  { category: 'storage', api: 'Volume.create / connect / list / destroy', status: 'partial', cube: 'cubesandbox.Volume', en: 'CRUD and mounts overlap. Cube uses operator-selected plugins and adds optional names and per-attachment read-only mode.', zh: 'CRUD 与挂载能力有交集。Cube 使用运维方选择的插件，并增加可选名称和单次挂载只读模式。' },
  { category: 'storage', api: 'volume.read_file / write_file / list', status: 'missing', cube: 'Mount, then sandbox.files', en: 'Cube has no standalone E2B volume-content service. Mount the volume and access it through sandbox.files.', zh: 'Cube 没有独立 E2B Volume 内容服务；应先挂载 Volume，再通过 sandbox.files 访问。' },
  { category: 'storage', api: 'Template.build() fluent API', status: 'mapped', cube: 'Cube Template / cubemastercli', en: 'The template systems are not wire-compatible. Cube builds from OCI images with its own build, alias, distribution, and compatibility APIs.', zh: '双方模板系统不在线协议层兼容。Cube 从 OCI 镜像构建，并提供自己的构建、别名、分发和兼容检查 API。' },
  { category: 'platform', api: 'sandbox.get_mcp_token()', status: 'missing', cube: 'Bake services into a template', en: 'Cube accepts mcp for wire tolerance but does not provision E2B MCP or return an MCP token.', zh: 'Cube 为线协议容错而接受 mcp 字段，但不会部署 E2B MCP，也不会返回 MCP token。' },
  { category: 'platform', api: 'Sandbox.create(iam=...)', status: 'missing', cube: 'CubeEgress injection', en: 'E2B workload identity is not implemented. Cube can keep static credentials outside the guest and inject headers at egress.', zh: 'Cube 未实现 E2B workload identity；Cube 可把静态凭据留在 Guest 外，并在出网时注入请求头。' },
  { category: 'platform', api: 'Secret.*', status: 'missing', cube: 'Deployment secret store', en: 'Cube has no E2B Secrets REST resource. Secret ownership and rotation belong to the deployment or CubeEgress.', zh: 'Cube 没有 E2B Secrets REST 资源；密钥所有权和轮换由部署系统或 CubeEgress 负责。' },
  { category: 'platform', api: 'AsyncSandbox / async resources', status: 'missing', cube: 'Node.js SDK / executor', en: 'E2B provides async Python. Cube Python is synchronous; Cube also ships native Node.js and Go SDKs.', zh: 'E2B 提供异步 Python；Cube Python 为同步 API，同时提供原生 Node.js 和 Go SDK。' },
  { category: 'extension', api: 'sandbox.rollback()', status: 'extension', cube: 'sandbox.rollback()', en: 'Cube restores filesystem and memory in place while retaining the sandbox ID. E2B has snapshots and forks but no equivalent in-place rollback method.', zh: 'Cube 可在保持沙箱 ID 不变的情况下原地恢复文件系统和内存；E2B 有快照和 fork，但没有等价的原地回滚方法。' },
  { category: 'extension', api: 'Sandbox.create(template=snapshot_id)', status: 'extension', cube: 'Sandbox.create(template=snapshot_id)', en: 'With an S3-backed, remotely ready snapshot, Cube can resume or create on another compatible node. The storage backend and host compatibility gates are Cube-specific.', zh: '使用 S3 后端且远端状态已就绪的快照时，Cube 可以在其他兼容节点恢复或创建沙箱；存储后端和宿主机兼容门禁是 Cube 特有机制。' },
  { category: 'extension', api: 'Volume.create(driver=...) / VolumeMount(read_only=True)', status: 'extension', cube: 'cubesandbox.Volume', en: 'Cube can select an operator-installed volume driver and make an individual attachment read-only. These options extend the shared E2B-style volume workflow.', zh: 'Cube 可以选择运维方安装的 Volume 驱动，并将某一次挂载设置为只读；这些参数扩展了双方共有的 Volume 工作流。' },
  { category: 'extension', api: 'sandbox.update_network(rules=...)', status: 'extension', cube: 'sandbox.update_network()', en: 'Cube adds ordered L7 match and action rules for host, SNI, port, method, and path, including deny, audit, header injection, and established-flow revocation.', zh: 'Cube 增加有序 L7 匹配和动作规则，可按 Host、SNI、端口、Method、Path 执行拒绝、审计、请求头注入和已有连接撤销。' },
  { category: 'extension', api: 'Sandbox.create(distribution_scope=...)', status: 'extension', cube: 'Sandbox.create()', en: 'Cube can restrict eligible compute nodes or pin a sandbox to one node. E2B does not expose equivalent placement control in its public SDK.', zh: 'Cube 可以限制沙箱可调度的计算节点，或将其固定到单个节点；E2B 公共 SDK 没有等价的放置控制。' },
  { category: 'extension', api: 'GET /v1/metrics/resource', status: 'extension', cube: 'Cubelet Prometheus endpoint', en: 'Cubelet exports separate host-MicroVM and guest-workload CPU and memory series for Prometheus, including guest metrics-epoch semantics.', zh: 'Cubelet 通过 Prometheus 分别导出宿主机 MicroVM 与 Guest 工作负载的 CPU、内存指标，并提供 Guest 指标周期语义。' }
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
  'Sandbox.create()': `from e2b import Sandbox

# The context manager kills the sandbox when the block exits.
with Sandbox.create(timeout=60) as sandbox:
    print(sandbox.sandbox_id)`,
  'Sandbox.connect()': `from e2b import Sandbox

sandbox = Sandbox.create(timeout=120)
try:
    connected = Sandbox.connect(sandbox.sandbox_id, timeout=60)
    print(connected.commands.run("echo connected").stdout)
finally:
    sandbox.kill()`,
  'Sandbox.list()': `from e2b import Sandbox

paginator = Sandbox.list(limit=10)
while paginator.has_next:
    for info in paginator.next_items():
        print(info.sandbox_id, info.state)`,
  'sandbox.get_info()': `from e2b import Sandbox

with Sandbox.create() as sandbox:
    info = sandbox.get_info()
    print(info.sandbox_id, info.state, info.started_at)`,
  'sandbox.is_running()': `from e2b import Sandbox

with Sandbox.create() as sandbox:
    print("running:", sandbox.is_running())`,
  'sandbox.kill()': `from e2b import Sandbox

sandbox = Sandbox.create()
sandbox_id = sandbox.sandbox_id
removed = sandbox.kill()
print(sandbox_id, removed)`,
  'sandbox.pause()': `from e2b import Sandbox

sandbox = Sandbox.create()
sandbox_id = sandbox.sandbox_id
sandbox.pause()  # Preserve state for a later resume.

resumed = Sandbox.connect(sandbox_id)
print(resumed.commands.run("echo resumed").stdout)
resumed.kill()`,
  'sandbox.set_timeout()': `from e2b import Sandbox

with Sandbox.create(timeout=60) as sandbox:
    # Replace the current timeout with five minutes.
    sandbox.set_timeout(300)
    print(sandbox.get_info().end_at)`,
  'sandbox.update_network()': `from e2b import Sandbox

with Sandbox.create() as sandbox:
    # Omitted fields are cleared because this replaces the policy.
    sandbox.update_network({"deny_out": ["10.0.0.0/8"]})
    print("network policy updated")`,
  'sandbox.get_metrics()': `from e2b import Sandbox

with Sandbox.create() as sandbox:
    sandbox.commands.run("python -c 'sum(range(1000000))'")
    for sample in sandbox.get_metrics():
        print(sample)`,
  'sandbox.commands.run()': `from e2b import Sandbox

with Sandbox.create() as sandbox:
    result = sandbox.commands.run(
        "python -c 'print(6 * 7)'",
        timeout=30,
    )
    print(result.stdout, result.exit_code)`,
  'commands.connect()': `from e2b import Sandbox

with Sandbox.create() as sandbox:
    process = sandbox.commands.run("sleep 2; echo done", background=True)
    pid = process.pid
    process.disconnect()  # The process keeps running in the sandbox.
    result = sandbox.commands.connect(pid).wait()
    print(result.stdout)`,
  'commands.list / kill / send_stdin': `from e2b import Sandbox

with Sandbox.create() as sandbox:
    process = sandbox.commands.run("cat", background=True, stdin=True)
    sandbox.commands.send_stdin(process.pid, "hello\\n")
    print([item.pid for item in sandbox.commands.list()])
    sandbox.commands.kill(process.pid)`,
  'files.read / write / write_files': `from e2b import Sandbox
from e2b.sandbox.filesystem.filesystem import WriteEntry

with Sandbox.create() as sandbox:
    sandbox.files.write("/tmp/one.txt", "one")
    sandbox.files.write_files([
        WriteEntry(path="/tmp/two.txt", data="two"),
        WriteEntry(path="/tmp/three.bin", data=b"three"),
    ])
    print(sandbox.files.read("/tmp/one.txt"))`,
  'files.list / exists / remove / rename / make_dir': `from e2b import Sandbox

with Sandbox.create() as sandbox:
    sandbox.files.make_dir("/tmp/demo")
    sandbox.files.write("/tmp/demo/a.txt", "hello")
    print(sandbox.files.exists("/tmp/demo/a.txt"))
    print([entry.name for entry in sandbox.files.list("/tmp/demo")])
    sandbox.files.rename("/tmp/demo/a.txt", "/tmp/demo/b.txt")
    sandbox.files.remove("/tmp/demo")`,
  'files.get_info()': `from e2b import Sandbox

with Sandbox.create() as sandbox:
    sandbox.files.write("/tmp/info.txt", "hello")
    info = sandbox.files.get_info("/tmp/info.txt")
    print(info.name, info.type, info.path)`,
  'files.watch_dir()': `from e2b import Sandbox

with Sandbox.create() as sandbox:
    sandbox.files.make_dir("/tmp/watched")
    watcher = sandbox.files.watch_dir("/tmp/watched")
    try:
        sandbox.files.write("/tmp/watched/new.txt", "hello")
        print(watcher.get_new_events())
    finally:
        watcher.stop()`,
  'sandbox.pty.*': `from e2b import PtySize, Sandbox

with Sandbox.create() as sandbox:
    terminal = sandbox.pty.create(PtySize(rows=24, cols=80))
    sandbox.pty.send_stdin(terminal.pid, b"echo hello from PTY\\n")
    sandbox.pty.resize(terminal.pid, PtySize(rows=30, cols=100))
    sandbox.pty.kill(terminal.pid)`,
  'sandbox.git.*': `from e2b import Sandbox

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
  'sandbox.run_code()': `from e2b_code_interpreter import Sandbox

# Install e2b-code-interpreter and use its matching version.
with Sandbox.create() as sandbox:
    execution = sandbox.run_code("x = 6 * 7; x")
    print(execution.results[0].text)`,
  'create / list / delete_snapshot': `from e2b import Sandbox

with Sandbox.create() as sandbox:
    sandbox.files.write("/tmp/state.txt", "snapshot me")
    snapshot = sandbox.create_snapshot(name="docs-example")

paginator = Sandbox.list_snapshots(name="docs-example")
while paginator.has_next:
    print([item.snapshot_id for item in paginator.next_items()])

Sandbox.delete_snapshot(snapshot.snapshot_id)`,
  'sandbox.fork()': `from e2b import Sandbox

with Sandbox.create() as parent:
    parent.files.write("/tmp/shared.txt", "before fork")
    children = parent.fork(count=2, timeout=60)
    for child in children:
        if isinstance(child, Exception):
            print("fork failed:", child)
            continue
        print(child.files.read("/tmp/shared.txt"))
        child.kill()`,
  'Volume.create / connect / list / destroy': `from uuid import uuid4
from e2b import Volume

volume = Volume.create(f"docs-{uuid4().hex[:8]}")
connected = Volume.connect(volume.volume_id)
print(connected.volume_id)
print([(item.volume_id, item.name) for item in Volume.list()])
Volume.destroy(volume.volume_id)`,
  'volume.read_file / write_file / list': `from uuid import uuid4
from e2b import Volume

volume = Volume.create(f"docs-{uuid4().hex[:8]}")
try:
    volume.write_file("/hello.txt", "hello", force=True)
    print(volume.read_file("/hello.txt"))
    print([entry.name for entry in volume.list("/")])
finally:
    Volume.destroy(volume.volume_id)`,
  'Template.build() fluent API': `from uuid import uuid4
from e2b import Template

template = (
    Template()
    .from_python_image("3.12")
    .run_cmd("python --version")
)
# Building can take several minutes and creates a project template.
Template.build(template, f"docs-python-{uuid4().hex[:8]}")`,
  'sandbox.get_mcp_token()': `from e2b import Sandbox

mcp = {
    "github/modelcontextprotocol/servers": {
        "run_cmd": "npx -y @modelcontextprotocol/server-everything"
    }
}
with Sandbox.create(mcp=mcp) as sandbox:
    print(sandbox.get_mcp_token())`,
  'Sandbox.create(iam=...)': `from e2b import Sandbox, Secret

identity = Secret.iam_token(
    audience="sts.amazonaws.com",
    token_type="JWT-SVID",
)
with Sandbox.create(iam={"tokens": {"aws": identity}}) as sandbox:
    print(sandbox.sandbox_id)`,
  'Secret.*': `from uuid import uuid4
from e2b import Secret

name = f"docs-{uuid4().hex[:8]}"
created = Secret.create(name, "initial-value")
Secret.update(name, "rotated-value")
print(Secret.get_info(name))  # Values are write-only.
print(Secret.list(limit=10).next_items())
Secret.destroy(created.secret_id)`,
  'AsyncSandbox / async resources': `import asyncio
from e2b import AsyncSandbox

async def main():
    sandbox = await AsyncSandbox.create()
    try:
        result = await sandbox.commands.run("echo async")
        print(result.stdout)
    finally:
        await sandbox.kill()

asyncio.run(main())`,
  'sandbox.rollback()': `from cubesandbox import Sandbox

with Sandbox.create() as sandbox:
    sandbox.files.write("/tmp/value.txt", "v1")
    snapshot = sandbox.create_snapshot()
    try:
        sandbox.files.write("/tmp/value.txt", "v2")
        sandbox.rollback(snapshot.snapshot_id)
        print(sandbox.files.read("/tmp/value.txt"))
    finally:
        Sandbox.delete_snapshot(snapshot.snapshot_id)`,
  'Sandbox.create(template=snapshot_id)': `from cubesandbox import Sandbox

with Sandbox.create() as source:
    source.files.write("/tmp/state.txt", "portable state")
    snapshot = source.create_snapshot()

try:
    # Cross-node placement requires an S3-backed, remotely ready snapshot.
    with Sandbox.create(template=snapshot.snapshot_id) as restored:
        print(restored.files.read("/tmp/state.txt"))
finally:
    Sandbox.delete_snapshot(snapshot.snapshot_id)`,
  'Volume.create(driver=...) / VolumeMount(read_only=True)': `from cubesandbox import Sandbox, Volume, VolumeMount

# CUBE_VOLUME_DRIVER must name a configured Cube volume plugin.
volume = Volume.create("docs-data", driver=os.environ["CUBE_VOLUME_DRIVER"])
try:
    with Sandbox.create(
        volume_mounts={"/data": VolumeMount(volume, read_only=True)}
    ) as sandbox:
        print(sandbox.files.list("/data"))
finally:
    Volume.destroy(volume.volume_id)`,
  'sandbox.update_network(rules=...)': `from cubesandbox import Action, Match, Rule, Sandbox

rule = Rule(
    name="allow-example-api",
    match=Match(host="api.example.com", path="/v1/"),
    action=Action(allow=True, audit="metadata"),
)
with Sandbox.create() as sandbox:
    sandbox.update_network({"rules": [rule]})`,
  'Sandbox.create(distribution_scope=...)': `from cubesandbox import Sandbox

# Use a compute-node ID or host IP reported by CubeOps.
with Sandbox.create(
    distribution_scope=[os.environ["CUBE_COMPUTE_NODE"]]
) as sandbox:
    print(sandbox.sandbox_id)`,
  'GET /v1/metrics/resource': `import os
from urllib.request import urlopen

cubelet_url = os.environ.get("CUBELET_URL", "http://127.0.0.1:9998")
url = f'{cubelet_url.rstrip("/")}/v1/metrics/resource'
with urlopen(url, timeout=5) as response:
    print(response.read().decode())`
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
  '# The base sandbox-code template does not include Git.': '# 基础 sandbox-code 模板默认不包含 Git。'
}

const pythonKeywords = new Set(['and', 'as', 'async', 'await', 'break', 'class', 'continue', 'def', 'del', 'elif', 'else', 'except', 'finally', 'for', 'from', 'global', 'if', 'import', 'in', 'is', 'lambda', 'nonlocal', 'not', 'or', 'pass', 'raise', 'return', 'try', 'while', 'with', 'yield'])
const pythonBuiltins = new Set(['Exception', 'False', 'None', 'True', 'bytes', 'dict', 'enumerate', 'f', 'int', 'isinstance', 'len', 'list', 'print', 'range', 'str', 'sum'])

function localizedExample(api) {
  let source = examples[api] || ''
  if (/\b(?:Async)?Sandbox\.create\(/.test(source)) {
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
      html += `<span class="py-number">${source.slice(index, end)}</span>`
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
      return `<span class="sh-keyword">${env[1]}</span> <span class="sh-variable">${env[2]}</span><span class="sh-operator">${env[3]}</span><span class="sh-string">${escapeHtml(env[4])}</span>`
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
let copyResetTimer
const t = computed(() => copy[props.locale] || copy.en)
const categories = ['lifecycle', 'runtime', 'storage', 'platform', 'extension']
const filteredRows = computed(() => {
  const needle = query.value.trim().toLowerCase()
  return rows.filter((row) => (activeCategory.value === 'all' || row.category === activeCategory.value) && (!needle || `${row.api} ${row.cube} ${row.en} ${row.zh} ${purposes[row.api]?.en} ${purposes[row.api]?.zh}`.toLowerCase().includes(needle)))
})
const groupedRows = computed(() => categories.map((category) => ({ category, rows: filteredRows.value.filter((row) => row.category === category) })).filter((group) => group.rows.length))
const counts = computed(() => Object.keys(t.value.statuses).map((status) => ({ status, count: rows.filter((row) => row.status === status).length })))

async function copyExample(api) {
  await copyCode(api, localizedExample(api))
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
  <div class="e2b-compat">
    <section class="compat-hero">
      <div class="compat-hero__content">
        <p class="compat-eyebrow">{{ t.eyebrow }}</p><h1>{{ t.title }}</h1>
        <p class="compat-tagline">{{ t.subtitle }}</p><p class="compat-intro">{{ t.intro }}</p>
      </div>
      <div class="compat-orbit" aria-hidden="true">
        <div class="compat-logo e2b"><img src="https://raw.githubusercontent.com/e2b-dev/E2B/main/readme-assets/logo-black.png" alt=""></div>
        <div class="compat-link"><i/><i/><i/></div>
        <div class="compat-logo cube"><img :src="cubeLogo" alt=""></div>
      </div>
    </section>

    <section class="compat-quickstart">
      <header>
        <h2>{{ t.quickstartTitle }}</h2>
        <p>{{ t.quickstartIntro }}</p>
      </header>
      <div class="quickstart-terminal">
        <pre><code v-html="highlightShell(setupCommands)"></code></pre>
      </div>
    </section>

    <section id="sdk-api-matrix" class="compat-section">
      <header class="section-heading"><div><h2>{{ t.matrixTitle }}</h2><p>{{ t.matrixIntro }}</p><p class="matrix-version-warning">{{ t.matrixVersionWarning }}</p><p class="example-note">{{ t.exampleNote }}</p></div>
        <div class="compat-stats"><div v-for="item in counts" :key="item.status"><strong>{{ item.count }}</strong><i :class="item.status"/><small>{{ t.statuses[item.status] }}</small></div></div>
      </header>
      <div class="compat-toolbar">
        <div class="compat-tabs"><button :class="{active: activeCategory === 'all'}" @click="activeCategory='all'">{{ t.all }}</button><button v-for="category in categories" :key="category" :class="{active: activeCategory === category}" @click="activeCategory=category">{{ t.categories[category] }}</button></div>
        <label class="compat-search"><span>⌕</span><input v-model="query" type="search" :placeholder="t.search"></label>
      </div>
      <div v-if="groupedRows.length" class="compat-matrix">
        <section v-for="group in groupedRows" :key="group.category" class="compat-group"><h3>{{ t.categories[group.category] }}</h3>
          <div class="compat-table"><div class="table-head"><span>{{ t.api }}</span><span>{{ t.purpose }}</span><span>{{ t.status }}</span><span>{{ t.difference }}</span></div>
            <div v-for="row in group.rows" :key="row.api" class="compat-row">
              <div class="api" :class="{ grouped: apiLabels(row.api).length > 1 }" :data-label="t.api"><code v-for="label in apiLabels(row.api)" :key="label">{{ label }}</code></div>
              <div class="purpose" :data-label="t.purpose">{{ purposes[row.api]?.[locale] || purposes[row.api]?.en }}</div>
              <div :data-label="t.status"><span :class="['status-pill', row.status]">{{ t.statuses[row.status] }}</span></div>
              <div class="detail" :data-label="t.difference">
                <p>{{ row[locale] || row.en }}</p>
                <details class="api-example">
                  <summary><span class="show-example">{{ t.example }} <b>↓</b></span><span class="hide-example">{{ t.hideExample }} <b>↑</b></span></summary>
                  <div class="code-shell">
                    <button type="button" class="copy-code" :class="{ copied: copiedApi === row.api }" @click="copyExample(row.api)">{{ copiedApi === row.api ? t.copiedCode : t.copyCode }}</button>
                    <pre><code v-html="highlightPython(localizedExample(row.api))"></code></pre>
                  </div>
                </details>
              </div>
            </div>
          </div>
        </section>
      </div><div v-else class="compat-empty">{{ t.noResults }}</div>
    </section>

    <section id="sources" class="compat-sources"><div><h2>{{ t.sourceTitle }}</h2><p>{{ t.sourceIntro }}</p></div><div class="source-links"><a href="https://github.com/TencentCloud/CubeSandbox/blob/164de95fc0ec649524e77c0a37960df0f50e0b83/openapi.yml">Cube OpenAPI ↗</a><a href="https://github.com/e2b-dev/E2B/blob/ccaf9fc0ffe6ac39c7ec786af7608ab1de19467b/spec/openapi.yml">E2B OpenAPI ↗</a><a href="https://github.com/e2b-dev/E2B/blob/ccaf9fc0ffe6ac39c7ec786af7608ab1de19467b/packages/python-sdk/e2b/sandbox_sync/main.py">E2B Python SDK ↗</a><a href="https://github.com/TencentCloud/CubeSandbox/blob/164de95fc0ec649524e77c0a37960df0f50e0b83/tests/e2e/sdk_compat/e2b-versions.txt">Validated versions ↗</a></div></section>
  </div>
</template>

<style>
.e2b-compatibility-page .VPDoc .container,.e2b-compatibility-page .VPDoc .content,.e2b-compatibility-page .VPDoc .content-container{max-width:1180px!important}.e2b-compatibility-page .VPDoc{padding-top:32px}
.e2b-compat{--green:#20c997;--blue:#4f7cff;--amber:#e99b24;--red:#e65367;--border:color-mix(in srgb,var(--vp-c-divider) 82%,transparent);--surface:color-mix(in srgb,var(--vp-c-bg-soft) 72%,var(--vp-c-bg));color:var(--vp-c-text-1)}
.e2b-compat h1,.e2b-compat h2,.e2b-compat h3,.e2b-compat p{margin:0;border:0}.compat-hero{position:relative;display:grid;grid-template-columns:minmax(0,1.35fr) minmax(280px,.65fr);min-height:390px;overflow:hidden;border:1px solid var(--border);border-radius:28px;background:linear-gradient(135deg,color-mix(in srgb,var(--vp-c-bg-alt) 88%,#101a32),var(--vp-c-bg) 58%);box-shadow:0 28px 80px rgba(12,20,38,.11)}
.compat-hero:after{position:absolute;inset:0;background-image:linear-gradient(var(--border) 1px,transparent 1px),linear-gradient(90deg,var(--border) 1px,transparent 1px);background-size:40px 40px;opacity:.2;content:'';mask-image:linear-gradient(to right,transparent,#000 55%)}.compat-hero__content{position:relative;z-index:2;align-self:center;padding:52px 28px 52px 52px}.compat-eyebrow,.section-heading em,.migration-panel em{color:var(--blue);font-size:12px;font-style:normal;font-weight:800;letter-spacing:.16em}.compat-hero h1{margin-top:14px;font-size:clamp(42px,6vw,72px);line-height:.98;letter-spacing:-.055em}.compat-tagline{margin-top:15px!important;font-size:clamp(20px,2.4vw,28px);font-weight:650;letter-spacing:-.025em}.compat-intro{max-width:650px;margin-top:16px!important;color:var(--vp-c-text-2);font-size:16px;line-height:1.7}.compat-baseline{display:inline-flex;flex-wrap:wrap;gap:8px 13px;align-items:center;margin-top:25px;padding:9px 13px;border:1px solid var(--border);border-radius:10px;background:color-mix(in srgb,var(--vp-c-bg) 76%,transparent);font-size:12px}.compat-baseline span{color:var(--vp-c-text-3);font-weight:700;text-transform:uppercase;letter-spacing:.08em}
.compat-orbit{position:relative;z-index:2;display:flex;align-items:center;justify-content:center;padding-right:42px}.compat-logo{display:grid;width:118px;height:108px;place-items:center;border:1px solid var(--border);border-radius:25px;background:rgba(255,255,255,.94);box-shadow:0 18px 45px rgba(22,36,78,.16);transform:rotate(-4deg)}.compat-logo img{display:block;max-width:78%;max-height:72%;object-fit:contain}.compat-logo.e2b img{width:86px}.compat-logo.cube{transform:rotate(4deg)}.compat-logo.cube img{height:82px}.compat-link{display:flex;width:62px;justify-content:space-around}.compat-link i{width:7px;height:7px;border-radius:50%;background:var(--vp-c-text-3)}
.compat-decisions{display:grid;grid-template-columns:1.25fr 1fr 1fr;gap:14px;margin-top:18px}.decision-card{display:flex;gap:14px;padding:22px;border:1px solid var(--border);border-radius:18px;background:var(--surface)}.decision-card.warning{border-color:color-mix(in srgb,var(--amber) 42%,var(--border));background:rgba(233,155,36,.11)}.decision-card>b{display:grid;flex:0 0 30px;height:30px;place-items:center;border-radius:9px;background:var(--vp-c-bg);color:var(--blue)}.decision-card.warning>b{color:var(--amber)}.decision-card h2{font-size:15px;line-height:1.35}.decision-card p{margin-top:8px;color:var(--vp-c-text-2);font-size:13px;line-height:1.55}
.compat-quickstart{margin-top:22px}.compat-quickstart>header{display:flex;gap:24px;align-items:flex-end;justify-content:space-between;margin-bottom:14px}.compat-quickstart>header h2{font-size:25px;letter-spacing:-.025em}.compat-quickstart>header p{max-width:650px;color:var(--vp-c-text-2);font-size:13px;line-height:1.6;text-align:right}.sdk-grid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:14px}.sdk-card{min-width:0;padding:23px;border:1px solid var(--border);border-radius:18px;background:var(--surface)}.sdk-card.e2b{border-top:3px solid #111827}.sdk-card.cube{border-top:3px solid var(--blue)}.sdk-card__head{display:flex;gap:16px;align-items:center;justify-content:space-between}.sdk-card__head span{color:var(--blue);font-size:10px;font-weight:800;letter-spacing:.1em;text-transform:uppercase}.sdk-card.e2b .sdk-card__head span{color:var(--vp-c-text-2)}.sdk-card__head h3{margin-top:3px;font-size:18px}.sdk-card__head>code{padding:7px 9px;border-radius:7px;background:var(--vp-code-bg);color:var(--vp-c-text-1);font-size:11px;white-space:nowrap}.sdk-card>p{margin-top:14px;color:var(--vp-c-text-2);font-size:13px;line-height:1.6}.sdk-card h4{margin:19px 0 8px;color:var(--vp-c-text-3);font-size:10px;letter-spacing:.1em;text-transform:uppercase}.sdk-card dl{margin:0;border-top:1px solid var(--border)}.sdk-card dl>div{display:grid;grid-template-columns:150px 1fr;gap:12px;padding:9px 0;border-bottom:1px solid var(--border)}.sdk-card dt,.sdk-card dd{margin:0}.sdk-card dt code{color:var(--vp-c-text-1);font-size:11px;font-weight:700}.sdk-card dd{color:var(--vp-c-text-2);font-size:11px;line-height:1.45}.quickstart-code pre{overflow:auto;margin:0;padding:15px 16px;border:1px solid #29344b;border-radius:0;background:#111827;line-height:1.55}.quickstart-code pre code{display:block;padding:0 62px 0 0;background:transparent;color:#dbe5f5;font-size:11px;white-space:pre}.quickstart-code .py-comment{color:#8290a8;font-style:italic}.quickstart-code .py-keyword{color:#c792ea;font-weight:650}.quickstart-code .py-builtin{color:#82aaff}.quickstart-code .py-function{color:#7fdbca}.quickstart-code .py-string{color:#c3e88d}.quickstart-code .py-number{color:#f78c6c}.quickstart-code .py-operator{color:#89ddff}
.compat-section{margin-top:72px}.section-heading{display:flex;gap:32px;align-items:flex-end;justify-content:space-between;margin-bottom:24px}.section-heading h2,.migration-panel h2,.compat-sources h2{margin-top:7px;font-size:30px;line-height:1.15;letter-spacing:-.035em}.section-heading p,.compat-sources p{max-width:720px;margin-top:10px;color:var(--vp-c-text-2);line-height:1.65}.compat-stats{display:flex;overflow:hidden;flex:0 0 auto;border:1px solid var(--border);border-radius:14px;background:var(--surface)}.compat-stats>div{display:grid;grid-template-columns:auto auto;column-gap:7px;align-items:center;min-width:94px;padding:12px 14px;border-left:1px solid var(--border)}.compat-stats>div:first-child{border-left:0}.compat-stats strong{font-size:19px}.compat-stats i{width:7px;height:7px;border-radius:50%}.compat-stats i.compatible{background:var(--green)}.compat-stats i.partial{background:var(--amber)}.compat-stats i.mapped{background:var(--blue)}.compat-stats i.missing{background:var(--red)}.compat-stats small{grid-column:1/-1;color:var(--vp-c-text-3);font-size:10px;font-weight:700}
.compat-toolbar{position:sticky;z-index:8;top:64px;display:flex;gap:16px;align-items:center;justify-content:space-between;margin-bottom:18px;padding:10px;border:1px solid var(--border);border-radius:16px;background:color-mix(in srgb,var(--vp-c-bg) 91%,transparent);box-shadow:0 10px 30px rgba(20,30,55,.07);backdrop-filter:blur(16px)}.compat-tabs{display:flex;gap:4px;overflow-x:auto}.compat-tabs button{flex:0 0 auto;padding:8px 11px;border:0;border-radius:9px;background:transparent;color:var(--vp-c-text-2);cursor:pointer;font-size:12px;font-weight:700}.compat-tabs button:hover{background:var(--vp-c-bg-soft)}.compat-tabs button.active{color:#fff;background:var(--blue)}.compat-search{display:flex;flex:0 1 285px;gap:7px;align-items:center;padding:7px 10px;border:1px solid var(--border);border-radius:9px;background:var(--vp-c-bg);color:var(--vp-c-text-3)}.compat-search input{min-width:0;width:100%;border:0;outline:0;background:transparent;color:var(--vp-c-text-1);font:inherit;font-size:12px}
.matrix-version-warning{margin-top:7px!important;color:var(--amber)!important;font-size:12px;font-weight:650;line-height:1.5!important}.example-note{margin-top:8px!important;color:var(--vp-c-text-3)!important;font-size:12px}.compat-group{margin-top:28px}.compat-group>h3{display:flex;gap:10px;align-items:center;margin:0 0 10px;font-size:15px}.compat-group>h3:before{width:4px;height:17px;border-radius:3px;background:linear-gradient(var(--blue),var(--green));content:''}.compat-table{overflow:hidden;border:1px solid var(--border);border-radius:16px;background:var(--vp-c-bg)}.table-head,.compat-row{display:grid;grid-template-columns:minmax(170px,.8fr) minmax(175px,.9fr) 130px minmax(330px,2fr)}.table-head{background:var(--vp-c-bg-soft);color:var(--vp-c-text-3);font-size:10px;font-weight:800;letter-spacing:.07em;text-transform:uppercase}.table-head span{padding:11px 15px}.compat-row>div{min-width:0;padding:17px 15px;border-top:1px solid var(--border)}.compat-row>div+div,.table-head span+span{border-left:1px solid var(--border)}.compat-row:hover>div{background:rgba(79,124,255,.045)}.compat-row code{padding:3px 6px;border-radius:6px;background:var(--vp-code-bg);color:var(--vp-c-text-1);font-size:12px;overflow-wrap:anywhere}.compat-row .api code{color:var(--vp-c-text-1);font-weight:700}.compat-row .purpose,.compat-row .detail{color:var(--vp-c-text-2);font-size:13px;line-height:1.58}.status-pill{display:inline-flex;align-items:center;padding:6px 10px;border:1px solid currentColor;border-radius:9px;box-shadow:0 2px 7px rgba(20,30,55,.05);font-size:11px;font-weight:750;white-space:nowrap}.status-pill.compatible{color:#168967;background:rgba(32,201,151,.09)}.status-pill.partial{color:#b96d09;background:rgba(233,155,36,.1)}.status-pill.mapped{color:#3d65d8;background:rgba(79,124,255,.09)}.status-pill.missing{color:#cc4054;background:rgba(230,83,103,.08)}.api-example{margin-top:4px}.api-example summary{display:inline-flex;color:var(--blue);cursor:pointer;font-size:12px;font-weight:750;list-style:none;user-select:none}.api-example summary::-webkit-details-marker{display:none}.api-example summary:hover{text-decoration:underline}.api-example summary b{margin-left:3px;font-size:11px}.api-example .hide-example{display:none}.api-example[open] .show-example{display:none}.api-example[open] .hide-example{display:inline}.api-example pre{overflow:auto;margin:7px 0 0;padding:15px 16px;border:1px solid #29344b;border-radius:8px;background:#111827;box-shadow:inset 3px 0 #4f7cff;line-height:1.55}.api-example pre code{padding:0;background:transparent;color:#dbe5f5;font-size:11px;white-space:pre}.api-example .py-comment{color:#8290a8;font-style:italic}.api-example .py-keyword{color:#c792ea;font-weight:650}.api-example .py-builtin{color:#82aaff}.api-example .py-function{color:#7fdbca}.api-example .py-string{color:#c3e88d}.api-example .py-number{color:#f78c6c}.api-example .py-operator{color:#89ddff}.compat-empty{padding:48px;border:1px dashed var(--border);border-radius:16px;color:var(--vp-c-text-3);text-align:center}
.timeout-panel{padding:32px;border:1px solid var(--border);border-radius:24px;background:linear-gradient(145deg,var(--surface),var(--vp-c-bg))}.timeout-compare{display:grid;grid-template-columns:1fr 1fr;gap:16px}.timeout-card{position:relative;overflow:hidden;padding:25px;border:1px solid var(--border);border-radius:18px;background:var(--vp-c-bg)}.timeout-card>span{color:var(--blue);font-size:11px;font-weight:850;letter-spacing:.16em}.timeout-card.cube>span{color:var(--green)}.timeout-card h3{margin-top:8px;font-size:20px}.timeout-card>code{display:inline-block;margin-top:16px;padding:8px 10px;border-radius:8px;background:var(--vp-code-bg);font-size:12px}.timeout-card p{margin-top:15px;color:var(--vp-c-text-2);font-size:13px;line-height:1.65}.timeline{display:flex;align-items:center;margin-top:24px;color:var(--vp-c-text-3);font-size:10px;font-weight:700;text-transform:uppercase}.timeline i{position:relative;flex:1;height:2px;margin:0 10px;background:linear-gradient(90deg,var(--blue),var(--amber))}.timeout-card.cube .timeline i{background:linear-gradient(90deg,var(--green),var(--amber))}.timeline i:after{position:absolute;top:-3px;right:0;width:8px;height:8px;border-radius:50%;background:var(--amber);content:''}.timeline b{color:var(--amber)}.timeout-footnote{margin-top:16px!important;color:var(--vp-c-text-3);font-size:12px;text-align:center}
.extension-grid{display:grid;grid-template-columns:repeat(3,1fr);gap:14px}.extension-card{position:relative;min-height:180px;padding:23px;border:1px solid var(--border);border-radius:18px;background:var(--surface);color:inherit!important;text-decoration:none!important;transition:.18s ease}.extension-card:hover{border-color:var(--blue);box-shadow:0 18px 35px rgba(20,30,55,.09);transform:translateY(-3px)}.extension-card>span{display:grid;width:38px;height:38px;place-items:center;border-radius:11px;background:linear-gradient(145deg,rgba(79,124,255,.12),rgba(32,201,151,.12));color:var(--blue);font-size:20px}.extension-card h3{margin-top:20px;font-size:17px}.extension-card p{margin-top:8px;color:var(--vp-c-text-2);font-size:13px;line-height:1.55}.extension-card>b{position:absolute;top:22px;right:22px;color:var(--vp-c-text-3)}
.migration-panel{display:grid;grid-template-columns:.65fr 1.35fr;gap:50px;padding:34px;border-radius:24px;background:#17233e;color:#f7f9ff}.migration-panel em{color:#79a1ff}.migration-panel ol{display:grid;margin:0;padding:0;list-style:none}.migration-panel li{display:grid;grid-template-columns:36px 1fr;gap:12px;align-items:baseline;padding:13px 0;border-bottom:1px solid rgba(255,255,255,.12)}.migration-panel li:last-child{border-bottom:0}.migration-panel li span{color:#79a1ff;font-family:var(--vp-font-family-mono);font-size:11px}.migration-panel li p{color:rgba(255,255,255,.76);font-size:13px;line-height:1.5}.compat-sources{display:flex;gap:35px;align-items:center;justify-content:space-between;margin:60px 0 20px;padding-top:28px;border-top:1px solid var(--border)}.compat-sources h2{font-size:20px}.compat-sources p{max-width:560px;font-size:12px}.source-links{display:flex;flex-wrap:wrap;gap:8px;justify-content:flex-end}.source-links a{padding:7px 10px;border:1px solid var(--border);border-radius:8px;color:var(--vp-c-text-2);font-size:11px;font-weight:700;text-decoration:none}.source-links a:hover{border-color:var(--blue);color:var(--blue)}
@media(max-width:960px){.compat-hero{grid-template-columns:1fr}.compat-hero__content{padding:42px}.compat-orbit{min-height:170px;padding:0 30px 38px}.compat-decisions{grid-template-columns:1fr}.section-heading{align-items:flex-start;flex-direction:column}.compat-stats{width:100%}.compat-stats>div{flex:1;min-width:0}.compat-toolbar{top:56px;align-items:stretch;flex-direction:column}.compat-search{flex-basis:auto}.table-head{display:none}.compat-row{grid-template-columns:minmax(180px,1fr) minmax(220px,1.3fr) 145px}.compat-row .detail{grid-column:1/-1;border-left:0}.extension-grid{grid-template-columns:repeat(2,1fr)}}
@media(max-width:640px){.e2b-compatibility-page .VPDoc{padding-top:18px}.compat-hero{min-height:0;border-radius:20px}.compat-hero__content{padding:30px 22px}.compat-hero h1{font-size:43px}.compat-orbit{min-height:140px}.compat-logo{width:92px;height:82px;border-radius:20px}.compat-logo.cube img{height:63px}.compat-link{width:46px}.compat-section{margin-top:52px}.section-heading h2,.migration-panel h2{font-size:26px}.compat-stats{display:grid;grid-template-columns:1fr 1fr}.compat-stats>div{border-top:1px solid var(--border);border-left:1px solid var(--border)}.compat-stats>div:nth-child(-n+2){border-top:0}.compat-stats>div:nth-child(odd){border-left:0}.compat-row{display:block;padding:16px;border-top:1px solid var(--border)}.compat-row:first-child{border-top:0}.compat-row>div{padding:0;border:0!important}.compat-row>div+div{margin-top:13px}.compat-row>div:before{display:block;margin-bottom:5px;color:var(--vp-c-text-3);content:attr(data-label);font-size:9px;font-weight:800;letter-spacing:.08em;text-transform:uppercase}.timeout-panel{padding:22px}.timeout-compare,.extension-grid,.migration-panel{grid-template-columns:1fr}.migration-panel{gap:25px;padding:26px 22px}.compat-sources{align-items:flex-start;flex-direction:column}.source-links{justify-content:flex-start}}
.table-head,.compat-row{grid-template-columns:minmax(225px,1fr) minmax(165px,.8fr) 130px minmax(320px,1.8fr)}.compat-row .api code{white-space:normal;overflow-wrap:anywhere}.compat-row .api.grouped code{display:block;width:fit-content;max-width:100%}.compat-row .api.grouped code+code{margin-top:6px}.compat-stats i.extension{background:#8b5cf6}.status-pill.extension{color:#7650d6;background:rgba(139,92,246,.1)}.code-shell{position:relative;margin-top:7px}.api-example pre{margin:0;box-shadow:none}.copy-code{position:absolute;z-index:2;top:8px;right:8px;padding:4px 9px;border:1px solid #3a465f;border-radius:4px;background:#1b263b;color:#b9c6da;cursor:pointer;font-family:var(--vp-font-family-base);font-size:10px;font-weight:700;line-height:1.4;transition:.15s ease}.copy-code:hover{border-color:#61749a;color:#fff;background:#24324b}.copy-code.copied{border-color:#2e9f7d;color:#8be0c4;background:#173b36}.api-example pre code{display:block;padding-right:62px}
.compat-quickstart>header{display:block;margin-bottom:12px}.compat-quickstart>header h2{font-size:22px}.compat-quickstart>header p{max-width:none;margin-top:6px;color:var(--vp-c-text-2);font-size:13px;line-height:1.6;text-align:left}.quickstart-terminal pre{overflow:auto;margin:0;padding:18px 20px;border:1px solid #29344b;border-radius:8px;background:#111827;color:#dbe5f5;line-height:1.65}.quickstart-terminal code{background:transparent;color:inherit;font-size:12px;white-space:pre}.quickstart-terminal .sh-command{color:#82aaff;font-weight:700}.quickstart-terminal .sh-subcommand{color:#c792ea}.quickstart-terminal .sh-keyword{color:#c792ea;font-weight:700}.quickstart-terminal .sh-variable{color:#7fdbca}.quickstart-terminal .sh-operator{color:#89ddff}.quickstart-terminal .sh-string{color:#c3e88d}
@media(max-width:960px){.compat-row{grid-template-columns:minmax(225px,1fr) minmax(210px,1.2fr) 145px}}
@media(max-width:960px){.sdk-grid{grid-template-columns:1fr}}
@media(max-width:640px){.compat-quickstart>header{align-items:flex-start;flex-direction:column}.compat-quickstart>header p{text-align:left}.sdk-card{padding:19px}.sdk-card__head{align-items:flex-start;flex-direction:column}.sdk-card dl>div{grid-template-columns:1fr;gap:4px}}
</style>
