// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.
//
// Scenario registry used by the SandboxCases page. The page groups case
// cards by scenario (sub-directory under examples/), shows a topology
// preview per scenario, and filters by category / search keyword.
//
// AI / LLM scenarios are intentionally NOT exported — they live behind the
// `hidden` flag on the Rust side and never appear in the API.

import {
  Boxes,
  Camera,
  CircuitBoard,
  FlaskConical,
  FolderOpen,
  GitBranch,
  Globe2,
  Layers,
  type LucideIcon,
  Network,
  Rocket,
  ShieldCheck,
  TimerReset,
} from 'lucide-react';

export type ExampleCategoryId =
  | 'basics'
  | 'filesystem'
  | 'lifecycle'
  | 'network'
  | 'browser'
  | 'image'
  | 'advanced';

export interface ExampleCategory {
  id: ExampleCategoryId;
  /** Translation key without namespace prefix. */
  i18nKey: string;
  icon: LucideIcon;
  /** Tailwind gradient classes for the card banner. */
  accent: string;
  /** Description used as the category heading hint. */
  hintZh: string;
  hintEn: string;
}

export const EXAMPLE_CATEGORIES: ExampleCategory[] = [
  {
    id: 'basics',
    i18nKey: 'categories.basics',
    icon: Rocket,
    accent: 'from-primary/20 via-primary/5 to-transparent',
    hintZh: '沙箱最常用 API：创建、运行、读文件、暂停。',
    hintEn: 'The most-used APIs: create, run, read files, pause.',
  },
  {
    id: 'filesystem',
    i18nKey: 'categories.filesystem',
    icon: FolderOpen,
    accent: 'from-cube-emerald/20 via-cube-emerald/5 to-transparent',
    hintZh: '把主机目录挂载进沙箱、或在沙箱内读写文件。',
    hintEn: 'Mount host directories and read / write inside the guest.',
  },
  {
    id: 'lifecycle',
    i18nKey: 'categories.lifecycle',
    icon: TimerReset,
    accent: 'from-cube-amber/20 via-cube-amber/5 to-transparent',
    hintZh: '快照、回滚、克隆：让沙箱像 Git 一样可分叉。',
    hintEn: 'Snapshot, rollback, clone: branch sandboxes like Git.',
  },
  {
    id: 'network',
    i18nKey: 'categories.network',
    icon: Network,
    accent: 'from-cube-violet/20 via-cube-violet/5 to-transparent',
    hintZh: '用 eBPF 数据面强制执行 allow / deny / 隔离策略。',
    hintEn: 'Enforce allow / deny / isolation through the eBPF datapath.',
  },
  {
    id: 'browser',
    i18nKey: 'categories.browser',
    icon: Globe2,
    accent: 'from-cube-cyan/20 via-cube-cyan/5 to-transparent',
    hintZh: '在沙箱里跑 Playwright + Chromium,直接 CDP 控制。',
    hintEn: 'Run Playwright + Chromium inside the guest, driven over CDP.',
  },
  {
    id: 'image',
    i18nKey: 'categories.image',
    icon: Layers,
    accent: 'from-cube-rose/20 via-cube-rose/5 to-transparent',
    hintZh: '基于自定义镜像(nginx 等)启动沙箱并验证。',
    hintEn: 'Boot a sandbox from a custom image (nginx, …) and verify.',
  },
  {
    id: 'advanced',
    i18nKey: 'categories.advanced',
    icon: CircuitBoard,
    accent: 'from-cube-violet/15 via-cube-violet/5 to-transparent',
    hintZh: '本地 Sidecar、Host 改写、e2b 兼容等高级玩法。',
    hintEn: 'Local sidecar, Host rewriting, e2b-compatible mode.',
  },
];

export type Plane = 'control' | 'data';
export type NodeKind = 'user' | 'control' | 'data' | 'vm' | 'store';

export interface ScenarioNode {
  id: string;
  labelZh: string;
  labelEn: string;
  plane: Plane;
  kind: NodeKind;
  descriptionZh: string;
  descriptionEn: string;
}

export interface ScenarioEdge {
  from: string;
  to: string;
  labelZh: string;
  labelEn: string;
  plane: Plane;
}

export interface ScenarioTopology {
  nodes: ScenarioNode[];
  edges: ScenarioEdge[];
}

export interface ScenarioFile {
  /** File id matching the Rust registry (without scenario prefix). */
  id: string;
  filename: string;
  titleZh: string;
  titleEn: string;
  descriptionZh: string;
  descriptionEn: string;
  language: 'python' | 'go' | 'bash' | 'javascript';
}

export interface ExampleScenario {
  id: string;
  titleZh: string;
  titleEn: string;
  descriptionZh: string;
  descriptionEn: string;
  category: ExampleCategoryId;
  icon: LucideIcon;
  /** Tailwind accent gradient for the left-rail group header. */
  accent: string;
  /** GitHub docs anchor; falls back to the repo root when unset. */
  docsAnchor?: string;
  topology: ScenarioTopology;
  files: ScenarioFile[];
  /** Associated store catalog item ID. When set, the frontend uses it
   *  to auto-select a matching template or prompt the user to install one. */
  storeItemId?: string;
}

// ─── Shared topology helpers ────────────────────────────────────────────────
//
// Architecture overview:
//
//   Control plane (orchestration):
//     User Script → CubeAPI → CubeMaster → Cubelet → CubeShim → CubeHypervisor → MicroVM
//                                CubeMaster ──端点映射──→ CubeProxy
//
//   Network plane:
//     Cubelet ──EnsureNetwork──→ Network Agent ──eBPF Policy──→ MicroVM
//
//   Data plane (runtime, inside MicroVM):
//     CubeAPI → CubeProxy → CubeRuntime
//     MicroVM ──Start Runtime──→ CubeRuntime

const SHARED_NODES: ScenarioNode[] = [
  // ── Control plane ──────────────────────────────────────────────
  { id: 'user', labelZh: '用户脚本', labelEn: 'User Script', plane: 'control', kind: 'user',
    descriptionZh: '用户页面上点击「运行」后发起的示例脚本调用。',
    descriptionEn: 'The example invocation triggered when you click Run.' },
  { id: 'cubeapi', labelZh: 'CubeAPI', labelEn: 'CubeAPI ', plane: 'control', kind: 'control',
    descriptionZh: 'HTTP 网关：校验请求 → 调度 CubeMaster 创建沙箱 → 代理数据到 CubeProxy。',
    descriptionEn: 'HTTP gateway: validates requests, schedules sandbox creation via CubeMaster, proxies data via CubeProxy.' },
  { id: 'cubemaster', labelZh: 'CubeMaster', labelEn: 'CubeMaster', plane: 'control', kind: 'control',
    descriptionZh: '调度器：根据模板和负载挑选 Cubelet 节点，下发创建 MicroVM。',
    descriptionEn: 'Scheduler: picks a Cubelet node based on template & load, then creates a MicroVM.' },
  { id: 'cubelet', labelZh: 'Cubelet', labelEn: 'Cubelet', plane: 'control', kind: 'control',
    descriptionZh: '节点代理：管理本机MicroVM完整生命周期（创建/销毁/暂停/恢复/快照）。',
    descriptionEn: 'Per-node agent: manages the full MicroVM lifecycle (create/destroy/pause/resume/snapshot).' },
  { id: 'network-agent', labelZh: 'Network Agent', labelEn: 'Network Agent', plane: 'data', kind: 'control',
    descriptionZh: '网络策略代理：为沙箱分配 IP/Port，注入 eBPF 网络策略（EnsureNetwork）。',
    descriptionEn: 'Network policy agent: allocates IP/Port, injects eBPF policies (EnsureNetwork).' },
  { id: 'cubeshim', labelZh: 'CubeShim', labelEn: 'CubeShim', plane: 'control', kind: 'control',
    descriptionZh: '容器运行时 shim：接收 Cubelet 指令，调用 Hypervisor 管理 MicroVM 生命周期。',
    descriptionEn: 'Container runtime shim: receives Cubelet commands, calls Hypervisor to manage MicroVM lifecycle.' },
  { id: 'cube-hypervisor', labelZh: 'CubeHypervisor', labelEn: 'CubeHypervisor', plane: 'control', kind: 'control',
    descriptionZh: '虚拟化管理层：通过 QEMU/KVM 创建和销毁 MicroVM 实例。',
    descriptionEn: 'Virtualization layer: creates and destroys MicroVM instances via QEMU/KVM.' },

  // ── Data plane ─────────────────────────────────────────────────
  { id: 'cubeproxy', labelZh: 'CubeProxy', labelEn: 'CubeProxy', plane: 'data', kind: 'control',
    descriptionZh: 'TLS 终结的反向代理：将外部请求通过 WSS 隧道转发到沙箱内的 CubeRuntime。',
    descriptionEn: 'TLS-terminating reverse proxy: forwards requests via WSS tunnel to in-sandbox CubeRuntime.' },
  { id: 'microvm', labelZh: 'KVM MicroVM', labelEn: 'KVM MicroVM', plane: 'data', kind: 'vm',
    descriptionZh: 'QEMU/KVM 微虚拟机：沙箱的运行时隔离边界，内部运行 envd 和用户工作负载。',
    descriptionEn: 'QEMU/KVM MicroVM: the sandbox isolation boundary, running envd and the user workload.' },
  { id: 'cube-runtime', labelZh: 'CubeRuntime', labelEn: 'CubeRuntime', plane: 'data', kind: 'data',
    descriptionZh: '沙箱运行时：在 MicroVM 内启动守护进程，提供进程管理、文件系统和代码执行接口。',
    descriptionEn: 'Sandbox runtime: starts the in-VM daemon, providing process management, filesystem and code execution.' },
];

const SHARED_EDGES: ScenarioEdge[] = [
  // ── Control plane: request orchestration ───────────────────────
  { from: 'user', to: 'cubeapi', labelZh: 'HTTPS', labelEn: 'HTTPS', plane: 'control' },
  { from: 'cubeapi', to: 'cubemaster', labelZh: 'gRPC', labelEn: 'gRPC', plane: 'control' },
  { from: 'cubemaster', to: 'cubelet', labelZh: 'gRPC', labelEn: 'gRPC', plane: 'control' },
  { from: 'cubemaster', to: 'cubeproxy', labelZh: '端点映射', labelEn: 'Endpoint Mapping', plane: 'control' },

  // ── Node plane: network + runtime shim ─────────────────────────
  { from: 'cubelet', to: 'network-agent', labelZh: 'EnsureNetwork', labelEn: 'EnsureNetwork', plane: 'data' },
  { from: 'cubelet', to: 'cubeshim', labelZh: 'Create VM', labelEn: 'Create VM', plane: 'control' },
  { from: 'cubeshim', to: 'cube-hypervisor', labelZh: '虚拟化管理', labelEn: 'Hypervisor Mgmt', plane: 'control' },
  { from: 'network-agent', to: 'microvm', labelZh: 'eBPF 策略', labelEn: 'eBPF Policy', plane: 'data' },
  { from: 'cube-hypervisor', to: 'microvm', labelZh: 'QMP / boot', labelEn: 'QMP / boot', plane: 'control' },
  { from: 'microvm', to: 'cube-runtime', labelZh: '启动运行时', labelEn: 'Start Runtime', plane: 'data' },

  // ── Data plane: runtime data flow ──────────────────────────────
  { from: 'cubeapi', to: 'cubeproxy', labelZh: 'HTTPS', labelEn: 'HTTPS', plane: 'data' },
  { from: 'cubeproxy', to: 'cube-runtime', labelZh: 'WSS 隧道', labelEn: 'WSS tunnel', plane: 'data' },
];

function clone<T>(arr: T[]): T[] {
  return arr.map((x) => ({ ...x }));
}

function cloneSharedTopology(): ScenarioTopology {
  return { nodes: clone(SHARED_NODES), edges: clone(SHARED_EDGES) };
}

// ─── Scenario topologies ────────────────────────────────────────────────────
//
// Each scenario builds on the shared base and adds/removes nodes & edges
// to reflect its unique architecture. The shared base already covers the
// standard control-plane + data-plane flow; scenarios only need to declare
// their differences.

function topologyQuickstart(): ScenarioTopology {
  // The standard topology is exactly the shared base — no additions needed.
  return cloneSharedTopology();
}

function topologyNetworkPolicy(): ScenarioTopology {
  // Network policy is enforced by Network Agent via eBPF — no extra hop needed.
  return cloneSharedTopology();
}

function topologyHostMount(): ScenarioTopology {
  const t = cloneSharedTopology();
  // Add a host directory that is bind-mounted into the MicroVM.
  t.nodes.push({
    id: 'hostdir',
    labelZh: '主机目录',
    labelEn: 'Host Directory',
    plane: 'data',
    kind: 'store',
    descriptionZh: '主机本地目录，通过 9p / virtiofs 挂载到沙箱内 /mnt。',
    descriptionEn: 'Host directory bind-mounted into the sandbox at /mnt via 9p / virtiofs.',
  });
  t.edges.push({
    from: 'hostdir',
    to: 'microvm',
    labelZh: '9p / virtiofs',
    labelEn: '9p / virtiofs',
    plane: 'data',
  });
  return t;
}

function topologyBrowser(): ScenarioTopology {
  const t = cloneSharedTopology();
  // Add Playwright + Chromium as workload under CubeRuntime.
  // VNC preview (Xvfb → x11vnc → websockify) is pre-installed in the
  // browser template image; the detail is folded into the Chromium
  // description to keep the topology focused on user-facing data flow.
  t.nodes.push(
    {
      id: 'playwright',
      labelZh: 'Playwright (CDP)',
      labelEn: 'Playwright (CDP)',
      plane: 'data',
      kind: 'data',
      descriptionZh: 'Python 客户端，通过 Chrome DevTools Protocol 控制 Chromium。',
      descriptionEn: 'Python client driving Chromium over Chrome DevTools Protocol.',
    },
    {
      id: 'chromium',
      labelZh: 'Chromium :9000',
      labelEn: 'Chromium :9000',
      plane: 'data',
      kind: 'data',
      descriptionZh: '沙箱内启用 CDP 的 Chromium，运行在 Xvfb 虚拟显示器上，通过 websockify :6080 提供 noVNC 实时预览。',
      descriptionEn: 'Chromium with CDP on Xvfb virtual display. Live desktop preview via websockify :6080 (noVNC).',
    },
  );
  t.edges.push(
    { from: 'cube-runtime', to: 'playwright', labelZh: 'exec', labelEn: 'exec', plane: 'data' },
    { from: 'playwright', to: 'chromium', labelZh: 'CDP WS', labelEn: 'CDP WS', plane: 'data' },
  );
  return t;
}

function topologySnapshot(): ScenarioTopology {
  const t = cloneSharedTopology();
  // Add the LVM snapshot store between Cubelet and MicroVM.
  // Snapshots outlive the sandbox and can be used for clone / rollback.
  t.nodes.push({
    id: 'snapshot',
    labelZh: '快照 (LVM)',
    labelEn: 'Snapshot (LVM)',
    plane: 'control',
    kind: 'store',
    descriptionZh: '根 LV 的写时复制快照，生命周期独立于沙箱，用于克隆和回滚。',
    descriptionEn: 'CoW snapshot of the root LV. Outlives the sandbox; used for clone & rollback.',
  });
  t.edges.push(
    { from: 'cubelet', to: 'snapshot', labelZh: 'lvcreate', labelEn: 'lvcreate', plane: 'control' },
    { from: 'snapshot', to: 'microvm', labelZh: 'rollback / clone', labelEn: 'rollback / clone', plane: 'control' },
  );
  return t;
}

function topologyNginx(): ScenarioTopology {
  const t = cloneSharedTopology();
  // Add nginx as workload under CubeRuntime.
  t.nodes.push({
    id: 'nginx',
    labelZh: 'nginx :80',
    labelEn: 'nginx :80',
    plane: 'data',
    kind: 'data',
    descriptionZh: '沙箱内的 nginx，托管自定义镜像里的静态文件。',
    descriptionEn: 'nginx serving static files from the custom image inside the sandbox.',
  });
  t.edges.push({
    from: 'cube-runtime',
    to: 'nginx',
    labelZh: 'exec',
    labelEn: 'exec',
    plane: 'data',
  });
  return t;
}

// ─── Scenario registry ──────────────────────────────────────────────────────
//
// 8 scenarios — none of them AI / LLM. The Rust handler enforces the same
// `hidden: true` filter so even an attacker who knows the IDs cannot reach
// these scenarios through the HTTP API.

export const EXAMPLE_SCENARIOS: ExampleScenario[] = [
  {
    id: 'code-sandbox-quickstart',
    titleZh: '沙箱快速上手',
    titleEn: 'Sandbox Quickstart',
    descriptionZh: '创建一个沙箱并把最常用的 API 都跑一遍：create / exec_code / cmd / read / pause。',
    descriptionEn: 'Create a sandbox and walk through the most-used APIs: create, exec_code, cmd, read, and pause.',
    category: 'basics',
    icon: Rocket,
    accent: 'from-primary/20 via-primary/5 to-transparent',
    topology: topologyQuickstart(),
    storeItemId: 'sandbox-code',
    files: [
      { id: 'create', filename: 'create.py', language: 'python',
        titleZh: '创建沙箱', titleEn: 'Create Sandbox',
        descriptionZh: '从一个模板创建沙箱，并读取它的元数据。',
        descriptionEn: 'Create a sandbox from a template and read its metadata.' },
      { id: 'exec_code', filename: 'exec_code.py', language: 'python',
        titleZh: '执行代码', titleEn: 'Execute Code',
        descriptionZh: '通过 Jupyter 内核在沙箱里运行 Python 代码。',
        descriptionEn: 'Run Python code inside the sandbox through the Jupyter kernel.' },
      { id: 'cmd', filename: 'cmd.py', language: 'python',
        titleZh: '执行 Shell', titleEn: 'Run Shell Command',
        descriptionZh: '在沙箱里执行 Shell 命令并捕获 stdout。',
        descriptionEn: 'Execute a shell command inside the sandbox and capture stdout.' },
      { id: 'read', filename: 'read.py', language: 'python',
        titleZh: '读 / 写文件', titleEn: 'Read / Write File',
        descriptionZh: '读写沙箱内的文件。',
        descriptionEn: 'Read and write files inside the sandbox.' },
      { id: 'pause', filename: 'pause.py', language: 'python',
        titleZh: '暂停与恢复', titleEn: 'Pause & Resume',
        descriptionZh: '冻结沙箱状态并在之后恢复。',
        descriptionEn: 'Freeze the sandbox memory and resume it later.' },
    ],
  },
  {
    id: 'network-policy',
    titleZh: '网络策略',
    titleEn: 'Network Policy',
    descriptionZh: '应用网络策略并验证连通性。eBPF 数据面会拦截非法流量。',
    descriptionEn: 'Apply network policies and verify connectivity. The eBPF datapath drops traffic that violates the policy.',
    category: 'network',
    icon: ShieldCheck,
    accent: 'from-cube-violet/20 via-cube-violet/5 to-transparent',
    topology: topologyNetworkPolicy(),
    storeItemId: 'sandbox-code',
    files: [
      { id: 'network_no_internet', filename: 'network_no_internet.py', language: 'python',
        titleZh: '无互联网', titleEn: 'No Internet',
        descriptionZh: '出站完全阻断的沙箱。',
        descriptionEn: 'Sandbox without outbound network access.' },
      { id: 'network_allowlist', filename: 'network_allowlist.py', language: 'python',
        titleZh: '白名单', titleEn: 'Allowlist',
        descriptionZh: '限制出网到显式 IP 列表。',
        descriptionEn: 'Restrict egress to an explicit list of IPs.' },
      { id: 'network_denylist', filename: 'network_denylist.py', language: 'python',
        titleZh: '黑名单', titleEn: 'Denylist',
        descriptionZh: '默认放行，仅 deny 命中项。',
        descriptionEn: 'Default-allow with explicit deny entries.' },
    ],
  },
  {
    id: 'host-mount',
    titleZh: '主机目录挂载',
    titleEn: 'Host Mount',
    descriptionZh: '把主机目录 bind-mount 进沙箱文件系统。',
    descriptionEn: 'Bind-mount a host directory into the sandbox filesystem.',
    category: 'filesystem',
    icon: FolderOpen,
    accent: 'from-cube-emerald/20 via-cube-emerald/5 to-transparent',
    topology: topologyHostMount(),
    storeItemId: 'sandbox-code',
    files: [
      { id: 'create_with_mount', filename: 'create_with_mount.py', language: 'python',
        titleZh: '挂载并创建', titleEn: 'Create With Mount',
        descriptionZh: '创建带主机目录挂载的沙箱。',
        descriptionEn: 'Create a sandbox with a host directory mounted at /mnt.' },
    ],
  },
  {
    id: 'browser-sandbox',
    titleZh: '浏览器沙箱',
    titleEn: 'Browser Sandbox',
    descriptionZh: '在沙箱里启动 Playwright + Chromium，用 CDP 控制。',
    descriptionEn: 'Drive a headless Chromium with Playwright over CDP.',
    category: 'browser',
    icon: Globe2,
    accent: 'from-cube-cyan/20 via-cube-cyan/5 to-transparent',
    topology: topologyBrowser(),
    storeItemId: 'sandbox-browser',
    files: [
      { id: 'browser', filename: 'browser.py', language: 'python',
        titleZh: 'Playwright + Chromium', titleEn: 'Playwright + Chromium',
        descriptionZh: '启动 Chromium 并跑一段 Playwright 脚本。',
        descriptionEn: 'Boot a sandbox with Chromium and run a Playwright script.' },
    ],
  },
  {
    id: 'snapshot-rollback-clone',
    titleZh: '快照 / 回滚 / 克隆',
    titleEn: 'Snapshot · Rollback · Clone',
    descriptionZh: '把沙箱像 Git 一样分叉：从快照克隆出 N 个，回滚到任意时间点。',
    descriptionEn: 'Branch sandboxes like Git: clone from a snapshot, roll back to any point.',
    category: 'lifecycle',
    icon: GitBranch,
    accent: 'from-cube-amber/20 via-cube-amber/5 to-transparent',
    topology: topologySnapshot(),
    storeItemId: 'sandbox-code',
    files: [
      { id: '01_create_snapshot', filename: '01_create_snapshot.py', language: 'python',
        titleZh: '01 创建快照', titleEn: '01 Create Snapshot',
        descriptionZh: '在运行中的沙箱上打快照。',
        descriptionEn: 'Capture a snapshot from a running sandbox.' },
      { id: '02_list_snapshots', filename: '02_list_snapshots.py', language: 'python',
        titleZh: '02 列出快照', titleEn: '02 List Snapshots',
        descriptionZh: '查看集群下的快照列表。',
        descriptionEn: 'List snapshots attached to the cluster.' },
      { id: '03_clone_from_snapshot', filename: '03_clone_from_snapshot.py', language: 'python',
        titleZh: '03 从快照克隆', titleEn: '03 Clone From Snapshot',
        descriptionZh: '用快照派生新沙箱。',
        descriptionEn: 'Create a new sandbox from a snapshot.' },
      { id: '04_state_preserved', filename: '04_state_preserved.py', language: 'python',
        titleZh: '04 状态保留', titleEn: '04 State Preserved',
        descriptionZh: '验证状态在克隆后仍然保留。',
        descriptionEn: 'Verify state survives the clone.' },
      { id: '05_snapshot_outlives_sandbox', filename: '05_snapshot_outlives_sandbox.py', language: 'python',
        titleZh: '05 快照独立', titleEn: '05 Snapshot Outlives',
        descriptionZh: '快照生命周期独立于源沙箱。',
        descriptionEn: 'Snapshot outlives its source sandbox.' },
      { id: '06_clone_n', filename: '06_clone_n.py', language: 'python',
        titleZh: '06 串行克隆 N 次', titleEn: '06 Clone N Times',
        descriptionZh: '依次克隆出 N 个沙箱。',
        descriptionEn: 'Spin up N clones in sequence.' },
      { id: '07_clone_concurrent', filename: '07_clone_concurrent.py', language: 'python',
        titleZh: '07 并发克隆', titleEn: '07 Clone Concurrently',
        descriptionZh: '并发克隆 N 个沙箱。',
        descriptionEn: 'Spin up N clones in parallel.' },
      { id: '08_fork_three_axis', filename: '08_fork_three_axis.py', language: 'python',
        titleZh: '08 三轴分叉', titleEn: '08 Fork Three-axis',
        descriptionZh: '从三个正交维度克隆 / 回滚。',
        descriptionEn: 'Three orthogonal dimensions of clone / rollback.' },
      { id: '09_rollback', filename: '09_rollback.py', language: 'python',
        titleZh: '09 回滚', titleEn: '09 Rollback',
        descriptionZh: '把沙箱回滚到之前的快照。',
        descriptionEn: 'Roll the sandbox back to a previous snapshot.' },
      { id: '10_rollback_then_continue', filename: '10_rollback_then_continue.py', language: 'python',
        titleZh: '10 回滚后继续', titleEn: '10 Rollback Then Continue',
        descriptionZh: '回滚后继续正常执行。',
        descriptionEn: 'Rollback, then resume normal execution.' },
      { id: '11_delete_snapshot', filename: '11_delete_snapshot.py', language: 'python',
        titleZh: '11 删除快照', titleEn: '11 Delete Snapshot',
        descriptionZh: '从集群里清理一个快照。',
        descriptionEn: 'Clean up a snapshot from the cluster.' },
      { id: 'clone_demo', filename: 'clone_demo.py', language: 'python',
        titleZh: '克隆 Demo', titleEn: 'Clone Demo',
        descriptionZh: '端到端克隆示例。',
        descriptionEn: 'End-to-end clone walkthrough.' },
      { id: 'rollback_demo', filename: 'rollback_demo.py', language: 'python',
        titleZh: '回滚 Demo', titleEn: 'Rollback Demo',
        descriptionZh: '端到端回滚示例。',
        descriptionEn: 'End-to-end rollback walkthrough.' },
    ],
  },
  {
    id: 'cubesandbox-base-nginx',
    titleZh: '自定义镜像 (nginx)',
    titleEn: 'Custom Image (nginx)',
    descriptionZh: '基于带 nginx 的自定义镜像启动沙箱并访问静态资源。',
    descriptionEn: 'Boot a sandbox from a custom image that runs nginx and reach its static files.',
    category: 'image',
    icon: Layers,
    accent: 'from-cube-rose/20 via-cube-rose/5 to-transparent',
    topology: topologyNginx(),
    storeItemId: 'sandbox-nginx',
    files: [
      { id: 'test_files', filename: 'test_files.py', language: 'python',
        titleZh: 'Test Files', titleEn: 'Test Files',
        descriptionZh: '通过代理访问 nginx 服务的文件。',
        descriptionEn: 'Reach the nginx-served files via the proxy.' },
    ],
  },
];

// ─── Helpers ────────────────────────────────────────────────────────────────

export function findScenario(id: string): ExampleScenario | undefined {
  return EXAMPLE_SCENARIOS.find((s) => s.id === id);
}

export function findFile(scenarioId: string, fileId: string): ScenarioFile | undefined {
  return findScenario(scenarioId)?.files.find((f) => f.id === fileId);
}

export function categoryMeta(id: ExampleCategoryId): ExampleCategory | undefined {
  return EXAMPLE_CATEGORIES.find((c) => c.id === id);
}

// Kept for CommandPalette / legacy callers that previously imported the
// `examples` data array from a different file.
export interface LegacyExampleEntry {
  id: string;
  scenario: string;
  filename: string;
  title: string;
  description: string;
  category: string;
  language: string;
}

export function buildLegacyExamples(): LegacyExampleEntry[] {
  const out: LegacyExampleEntry[] = [];
  for (const sc of EXAMPLE_SCENARIOS) {
    for (const f of sc.files) {
      out.push({
        id: `${sc.id}:${f.id}`,
        scenario: sc.id,
        filename: f.filename,
        title: f.titleEn,
        description: f.descriptionEn,
        category: sc.category,
        language: f.language,
      });
    }
  }
  return out;
}

// Lightweight type alias to keep imports consistent across the project.
export type { Boxes, Camera };