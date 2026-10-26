<script setup>
import { computed } from 'vue'
import { useRoute } from 'vitepress'

const route = useRoute()
const isZh = computed(() => route.path.startsWith('/zh/'))

const metaBarData = computed(() => isZh.value ? {
  tag: 'CubeSandbox',
  engine: '基于 RustVMM 与 KVM 内核构建',
  badges: ['Apache-2.0 开源协议', 'CNCF Landscape 项目', 'E2B SDK 兼容']
} : {
  tag: 'CubeSandbox',
  engine: 'Engineered with RustVMM & KVM',
  badges: ['Apache-2.0 License', 'CNCF Landscape', 'E2B SDK Compatible']
})

const benchmarkMetrics = computed(() => isZh.value ? [
  {
    value: '< 60ms',
    label: '极速冷启动',
    detail: '快照池化克隆，跳过内核引导耗时'
  },
  {
    value: '< 5MB',
    label: '单沙箱内存底噪',
    detail: '内存写时复制共享，单机数千并发'
  },
  {
    value: '1:1 MicroVM',
    label: '硬件级强隔离',
    detail: '每个沙箱专属轻量独立操作系统内核'
  },
  {
    value: 'Drop-in',
    label: 'E2B SDK 兼容平替',
    detail: '兼容 E2B SDK 接口，替换环境变量即可平滑切换'
  }
] : [
  {
    value: '< 60ms',
    label: 'Instant Cold Start',
    detail: 'Snapshot clones bypass kernel boot overhead'
  },
  {
    value: '< 5MB',
    label: 'Memory Overhead',
    detail: 'CoW page sharing, thousands per node'
  },
  {
    value: '1:1 MicroVM',
    label: 'Hardware Isolation',
    detail: 'Dedicated guest OS kernel per sandbox'
  },
  {
    value: 'Drop-in',
    label: 'E2B SDK Compatible',
    detail: 'Compatible with E2B SDK, switch seamlessly via env var'
  }
])

const headerData = computed(() => isZh.value ? {
  title: '专为 AI Agent 构建的下一代虚拟化沙箱底座',
  description: '从内核级 MicroVM 硬件隔离到百毫秒级内存写时复制分叉，以确定性安全与极低资源开销支撑海量自主智能体并发执行。'
} : {
  title: 'Next-Gen Virtualized Sandbox Infrastructure for AI Agents',
  description: 'From hardware-isolated MicroVM kernels to sub-100ms Copy-on-Write memory forks, engineered for deterministic, multi-tenant agent execution.'
})

const features = computed(() => isZh.value ? [
  {
    id: 'startup',
    index: '01',
    category: 'SYS_CORE // SNAPSHOT_ENGINE',
    metric: 'LATENCY: SUB_60ms // COLD_START',
    title: '极速冷启动与内存快照分叉',
    summary: '基于内核写时复制（CoW）与轻量内存池化预热，跳过传统虚机及系统内核引导损耗。单次快照克隆与沙箱拉起压缩在数十毫秒以内，实现智能体即开即用的瞬态并发执行。',
    link: '/zh/guide/snapshot-rollback-clone',
    action: '查看快照与克隆架构',
    spanClass: 'grid-span-7'
  },
  {
    id: 'isolation',
    index: '02',
    category: 'SYS_ISOLATION // HARDWARE_VIRT',
    metric: 'BOUNDARY: RING_0_DEDICATED',
    title: '硬件级 MicroVM 独立内核隔离',
    summary: '每个沙箱独享专属轻量操作系统内核（Guest OS），在宿主机物理边界与 KVM 虚拟化屏障内严格隔离，彻底隔绝沙箱内部未经审查代码的提权与跨租户逃逸。',
    link: '/zh/architecture/overview',
    action: '查看架构设计与边界',
    spanClass: 'grid-span-5'
  },
  {
    id: 'e2b',
    index: '03',
    category: 'SYS_COMPAT // E2B_SDK',
    metric: 'API: E2B_COMPATIBLE // DROP_IN',
    title: 'E2B SDK 接口兼容平替',
    summary: '对外兼容 E2B SDK 接口，替换一个环境变量即可从 E2B 云端无缝切换至私有化沙箱，零业务客户端代码改动。',
    link: '/zh/guide/tutorials/sdk/python',
    action: '查看 SDK 使用教程',
    spanClass: 'grid-span-4'
  },
  {
    id: 'network',
    index: '04',
    category: 'SYS_NET // EBPF_L7_GATEWAY',
    metric: 'FILTER: TC_EGRESS // SECRET_SHIELD',
    title: 'eBPF 内核网络与安全凭据代理',
    summary: '内核层挂载 eBPF 探针执行沙箱间无损硬隔离；内置 L7 安全反向代理支持精细化域名/路径鉴权与凭据动态注入，API 密钥全程对沙箱内执行代码物理不可见。',
    link: '/zh/guide/network-policy',
    action: '查看网络安全策略',
    spanClass: 'grid-span-4'
  },
  {
    id: 'density',
    index: '05',
    category: 'SYS_DENSITY // MEMORY_COW',
    metric: 'DENSITY: 1000+',
    title: 'MB 级超低开销与高密度驻留',
    summary: '通过内核空间共享与写时复制技术将单个沙箱内存开销压缩至 MB 级。单台物理机可高密度稳定运行数千个沙箱实例，搭配自动挂起唤醒实现极致资源利用。',
    link: '/zh/blog/posts/2026-06-01-cubesandbox-perf-benchmark',
    action: '查看性能基准测试报告',
    spanClass: 'grid-span-4'
  },
  {
    id: 'state',
    index: '06',
    category: 'SYS_STATE // DELTA_TREE',
    metric: 'CHECKPOINT: DELTA_TREE',
    title: '多维状态时光机与分支回退',
    summary: '毫秒级捕获沙箱运行时增量快照。支持任意检查点双向无损回退或分叉出多条平行探索分支，专为复杂智能体决策树验证与强化学习训练设计。',
    link: '/zh/guide/snapshot-rollback-clone',
    action: '查看状态分叉原理',
    spanClass: 'grid-span-3'
  },
  {
    id: 'volume',
    index: '07',
    category: 'SYS_STORAGE // VOLUME_BUS',
    metric: 'STORAGE: DECOUPLED_MOUNT',
    title: '解耦式 Volume 存储插件框架',
    summary: '存储生命周期与沙箱运行时彻底解耦，提供标准 S3、本地 Host Mount 等可插拔驱动插件，支持独立生命周期管理、热插拔挂载与多沙箱协同读写。',
    link: '/zh/guide/volume-plugin',
    action: '查看 Volume 插件开发',
    spanClass: 'grid-span-3'
  },
  {
    id: 'deploy',
    index: '08',
    category: 'SYS_INFRA // MULTI_NODE',
    metric: 'TOPOLOGY: MULTI_NODE',
    title: '多机集群部署与弹性编排',
    summary: '支持将单机沙箱平滑扩展为多机集群，通过控制节点（CubeMaster）集中调度与纳管分布式计算节点，兼顾高可用性与轻量运维。',
    link: '/zh/guide/multi-node-deploy',
    action: '查看多机集群部署',
    spanClass: 'grid-span-3'
  },
  {
    id: 'arm',
    index: '09',
    category: 'SYS_ARCH // AARCH64_NATIVE',
    metric: 'ISA: ARM64_FULL_STACK',
    title: 'ARM64 全栈原生指令支持',
    summary: '针对 ARM64 指令集全链路深度优化，从底层微虚拟化层、轻量操作系统内核到镜像构建与自动化部署全流程 100% 原生运行，提供顶尖能效比。',
    link: '/zh/blog/posts/2026-07-03-cubesandbox-v0.5.0-release#二、arm-原生支持-正式开启-双轨架构-时代',
    action: '查看 ARM 原生支持解读',
    spanClass: 'grid-span-3'
  }
] : [
  {
    id: 'startup',
    index: '01',
    category: 'SYS_CORE // SNAPSHOT_ENGINE',
    metric: 'LATENCY: SUB_60ms // COLD_START',
    title: 'Sub-60ms Cold Start & Memory Snapshot Forking',
    summary: 'Pre-warmed memory pooling combined with Copy-on-Write micro-clones eliminates standard kernel boot overhead. Sub-60ms instance instantiation powers instant, deterministic agent workflows.',
    link: '/guide/snapshot-rollback-clone',
    action: 'Explore Snapshot & Clone',
    spanClass: 'grid-span-7'
  },
  {
    id: 'isolation',
    index: '02',
    category: 'SYS_ISOLATION // HARDWARE_VIRT',
    metric: 'BOUNDARY: RING_0_DEDICATED',
    title: 'Hardware-level MicroVM Kernel Isolation',
    summary: 'Every sandbox executes within its own dedicated Linux kernel inside a lightweight MicroVM boundary, physically preventing container breakouts and malicious host compromise.',
    link: '/architecture/overview',
    action: 'Explore Architecture Boundary',
    spanClass: 'grid-span-5'
  },
  {
    id: 'e2b',
    index: '03',
    category: 'SYS_COMPAT // E2B_SDK',
    metric: 'API: E2B_COMPATIBLE // DROP_IN',
    title: 'E2B SDK Drop-in Compatible',
    summary: 'Compatible with E2B SDK interface. Switch from E2B Cloud seamlessly by changing one environment variable — zero client code changes.',
    link: '/guide/tutorials/sdk/python',
    action: 'Explore SDK Tutorial',
    spanClass: 'grid-span-4'
  },
  {
    id: 'network',
    index: '04',
    category: 'SYS_NET // EBPF_L7_GATEWAY',
    metric: 'FILTER: TC_EGRESS // SECRET_SHIELD',
    title: 'eBPF Kernel Egress & L7 Credential Proxy',
    summary: 'Kernel-level eBPF TC hooks enforce strict inter-sandbox isolation. Built-in L7 security reverse proxy manages route authorization and automatic credential injection without code exposure.',
    link: '/guide/network-policy',
    action: 'Explore Network Security',
    spanClass: 'grid-span-4'
  },
  {
    id: 'density',
    index: '05',
    category: 'SYS_DENSITY // MEMORY_COW',
    metric: 'DENSITY: 1000+',
    title: 'MB-level Footprint & Extreme Host Density',
    summary: 'Through shared read-only kernel structures and memory pages, per-sandbox overhead drops to single-digit MBs, supporting thousands of resident instances with auto-sleep/resume.',
    link: '/blog/posts/2026-06-01-cubesandbox-perf-benchmark',
    action: 'Explore Performance Benchmarks',
    spanClass: 'grid-span-4'
  },
  {
    id: 'state',
    index: '06',
    category: 'SYS_STATE // DELTA_TREE',
    metric: 'CHECKPOINT: DELTA_TREE',
    title: 'State Time Machine & Tree Forking',
    summary: 'Millisecond checkpoints capture point-in-time runtime deltas. Roll back seamlessly or fork divergent exploration paths for complex agent decision-tree validation.',
    link: '/guide/snapshot-rollback-clone',
    action: 'Explore State Branching',
    spanClass: 'grid-span-3'
  },
  {
    id: 'volume',
    index: '07',
    category: 'SYS_STORAGE // VOLUME_BUS',
    metric: 'STORAGE: DECOUPLED_MOUNT',
    title: 'Decoupled Volume Storage Framework',
    summary: 'Storage lifecycles operate independently from sandbox instances. Pluggable drivers support S3, host mounts, and shared block volumes with seamless hotplug.',
    link: '/guide/volume-plugin',
    action: 'Explore Volume Plugins',
    spanClass: 'grid-span-3'
  },
  {
    id: 'deploy',
    index: '08',
    category: 'SYS_INFRA // MULTI_NODE',
    metric: 'TOPOLOGY: MULTI_NODE',
    title: 'Multi-Node Cluster Deployment',
    summary: 'Seamlessly scale single-node sandboxes to a multi-node cluster. Centralized CubeMaster orchestrates compute nodes with high availability and minimal operational overhead.',
    link: '/guide/multi-node-deploy',
    action: 'Explore Multi-Node Deployment',
    spanClass: 'grid-span-3'
  },
  {
    id: 'arm',
    index: '09',
    category: 'SYS_ARCH // AARCH64_NATIVE',
    metric: 'ISA: ARM64_FULL_STACK',
    title: 'Native ARM64 Architecture Pipeline',
    summary: 'Deep optimization for AArch64 instruction sets spanning the custom hypervisor, minimal kernel, and OCI image execution, delivering peak density and efficiency.',
    link: '/blog/posts/2026-07-03-cubesandbox-v0.5.0-release#_2-arm-native-support-officially-entering-the-dual-track-architecture-era',
    action: 'Explore ARM Native Support',
    spanClass: 'grid-span-3'
  }
])
</script>

<template>
  <section class="blueprint-section" aria-labelledby="blueprint-heading">
    <div class="blueprint-container">
      
      <!-- Top Architecture & Trust Meta Header -->
      <div class="blueprint-meta-bar">
        <div class="blueprint-meta-group">
          <span class="meta-tag meta-tag--primary">{{ metaBarData.tag }}</span>
          <span class="meta-engine">{{ metaBarData.engine }}</span>
        </div>
        <div class="blueprint-trust-group" aria-hidden="true">
          <span v-for="badge in metaBarData.badges" :key="badge" class="trust-badge">
            <span class="trust-dot" />
            {{ badge }}
          </span>
        </div>
      </div>

      <div class="blueprint-intro">
        <h2 id="blueprint-heading" class="blueprint-title">{{ headerData.title }}</h2>
        <p class="blueprint-description">{{ headerData.description }}</p>
      </div>

      <!-- 4-Pillar Hardcore Benchmark Metrics Strip -->
      <div class="benchmark-strip" role="region" :aria-label="isZh ? '核心性能基准指标' : 'Core Performance Benchmarks'">
        <div
          v-for="metric in benchmarkMetrics"
          :key="metric.label"
          class="benchmark-item"
        >
          <div class="benchmark-val-box">
            <span class="benchmark-val">{{ metric.value }}</span>
          </div>
          <div class="benchmark-label">{{ metric.label }}</div>
          <div class="benchmark-sub">{{ metric.detail }}</div>
        </div>
      </div>

      <!-- Monolithic Blueprint Chassis -->
      <div class="blueprint-chassis">
        <!-- Corner Crosshairs -->
        <span class="chassis-crosshair ch-top-left" aria-hidden="true">+</span>
        <span class="chassis-crosshair ch-top-right" aria-hidden="true">+</span>
        <span class="chassis-crosshair ch-bottom-left" aria-hidden="true">+</span>
        <span class="chassis-crosshair ch-bottom-right" aria-hidden="true">+</span>

        <div class="blueprint-grid">
          <a
            v-for="item in features"
            :key="item.id"
            :href="item.link"
            class="cell-panel"
            :class="[item.spanClass, `cell--${item.id}`]"
          >
            <!-- Cell Border Flare on Hover -->
            <div class="cell-border-glow" aria-hidden="true" />

            <!-- Cell Header Plate -->
            <div class="cell-header">
              <div class="cell-index-box">
                <span class="cell-index">{{ item.index }}</span>
                <span class="cell-category">{{ item.category }}</span>
              </div>
              <div class="cell-metric-box">
                <span class="cell-metric">{{ item.metric }}</span>
              </div>
            </div>

            <!-- Technical Vector Schematic Graphic -->
            <div class="cell-schematic" aria-hidden="true">
              <!-- Schematic 01: Memory Baseline & COW Branch Topology -->
              <svg v-if="item.id === 'startup'" viewBox="0 0 520 120" fill="none" class="schematic-svg">
                <!-- Time Scale Axis -->
                <line x1="20" y1="20" x2="500" y2="20" stroke="currentColor" stroke-width="1" stroke-opacity="0.25" />
                <line x1="20" y1="16" x2="20" y2="24" stroke="currentColor" stroke-width="1" stroke-opacity="0.5" />
                <line x1="140" y1="17" x2="140" y2="23" stroke="currentColor" stroke-width="1" stroke-opacity="0.3" />
                <line x1="260" y1="17" x2="260" y2="23" stroke="currentColor" stroke-width="1" stroke-opacity="0.3" />
                <line x1="380" y1="17" x2="380" y2="23" stroke="currentColor" stroke-width="1" stroke-opacity="0.3" />
                <line x1="500" y1="16" x2="500" y2="24" stroke="currentColor" stroke-width="1" stroke-opacity="0.5" />
                <text x="20" y="12" fill="currentColor" fill-opacity="0.45" font-size="8" font-family="monospace">0ms</text>
                <text x="140" y="12" fill="currentColor" fill-opacity="0.45" font-size="8" font-family="monospace">15ms</text>
                <text x="260" y="12" fill="currentColor" fill-opacity="0.45" font-size="8" font-family="monospace">30ms</text>
                <text x="380" y="12" fill="currentColor" fill-opacity="0.45" font-size="8" font-family="monospace">45ms</text>
                <text x="460" y="12" fill="currentColor" fill-opacity="0.75" font-size="8" font-family="monospace">&lt;60ms COLD</text>

                <!-- Baseline Memory Track -->
                <rect x="20" y="38" width="130" height="20" stroke="currentColor" stroke-width="1" stroke-opacity="0.6" fill="currentColor" fill-opacity="0.04" />
                <text x="28" y="51" fill="currentColor" fill-opacity="0.9" font-size="9" font-family="monospace" font-weight="bold">MEM_POOL_BASE</text>
                
                <!-- Bus connection line -->
                <path d="M150 48 H210" stroke="currentColor" stroke-width="1" stroke-opacity="0.6" />
                <circle cx="210" cy="48" r="2.5" fill="currentColor" />

                <!-- 3 COW Forked Branches -->
                <!-- Branch A -->
                <path d="M210 48 V38 H270" stroke="currentColor" stroke-width="1" stroke-dasharray="2 2" stroke-opacity="0.6" />
                <rect x="270" y="28" width="160" height="18" stroke="currentColor" stroke-width="1" stroke-opacity="0.5" />
                <text x="278" y="40" fill="currentColor" fill-opacity="0.8" font-size="8.5" font-family="monospace">INST_01: ΔMEM +1.8MB [18ms]</text>

                <!-- Branch B -->
                <path d="M210 48 H270" stroke="currentColor" stroke-width="1" stroke-opacity="0.8" />
                <rect x="270" y="52" width="160" height="18" stroke="currentColor" stroke-width="1" stroke-opacity="0.5" />
                <text x="278" y="64" fill="currentColor" fill-opacity="0.8" font-size="8.5" font-family="monospace">INST_02: ΔMEM +2.4MB [22ms]</text>

                <!-- Branch C -->
                <path d="M210 48 V86 H270" stroke="currentColor" stroke-width="1" stroke-dasharray="2 2" stroke-opacity="0.6" />
                <rect x="270" y="76" width="160" height="18" stroke="currentColor" stroke-width="1" stroke-opacity="0.5" />
                <text x="278" y="88" fill="currentColor" fill-opacity="0.8" font-size="8.5" font-family="monospace">INST_03: ΔMEM +1.4MB [27ms]</text>

                <!-- Signal Arrow -->
                <path d="M440 61 L460 61 L455 57 M460 61 L455 65" stroke="currentColor" stroke-width="1" stroke-opacity="0.6" />
                <text x="468" y="64" fill="currentColor" fill-opacity="0.65" font-size="8" font-family="monospace">READY</text>
              </svg>

              <!-- Schematic 02: Hardware-level MicroVM Dual Boundary -->
              <svg v-if="item.id === 'isolation'" viewBox="0 0 340 120" fill="none" class="schematic-svg">
                <!-- Outer Host Boundary (Octagon / Beveled Frame) -->
                <path d="M40 15 H300 L320 35 V85 L300 105 H40 L20 85 V35 Z" stroke="currentColor" stroke-width="1" stroke-opacity="0.35" stroke-dasharray="4 2" />
                <text x="45" y="27" fill="currentColor" fill-opacity="0.4" font-size="7.5" font-family="monospace">HOST_BARRIER: DEV_KVM_IOCTL</text>
                
                <!-- Intermediate Interception Shield -->
                <rect x="50" y="38" width="240" height="58" stroke="currentColor" stroke-width="1" stroke-opacity="0.5" fill="currentColor" fill-opacity="0.03" />
                <line x1="50" y1="48" x2="290" y2="48" stroke="currentColor" stroke-width="0.8" stroke-opacity="0.25" />
                <text x="58" y="45" fill="currentColor" fill-opacity="0.6" font-size="7.5" font-family="monospace">HYPERVISOR_MEM_SHIELD // TRAP_VECTOR</text>

                <!-- Inner Ring-0 Guest OS Kernel Space -->
                <rect x="70" y="56" width="200" height="32" stroke="currentColor" stroke-width="1" stroke-opacity="0.8" fill="currentColor" fill-opacity="0.06" />
                <circle cx="85" cy="72" r="3" fill="currentColor" fill-opacity="0.8" />
                <text x="96" y="70" fill="currentColor" font-size="8.5" font-family="monospace" font-weight="bold">DEDICATED_GUEST_KERNEL</text>
                <text x="96" y="81" fill="currentColor" fill-opacity="0.5" font-size="7" font-family="monospace">RING_0 // CR3:0x7FFF // ZERO_SHARE</text>
              </svg>

              <!-- Schematic 03: E2B Wire Protocol Interface Pinout -->
              <svg v-if="item.id === 'e2b'" viewBox="0 0 280 88" fill="none" class="schematic-svg">
                <!-- E2B Standard Client Side -->
                <rect x="15" y="16" width="95" height="56" stroke="currentColor" stroke-width="1" stroke-opacity="0.5" fill="currentColor" fill-opacity="0.03" />
                <text x="22" y="30" fill="currentColor" fill-opacity="0.85" font-size="8" font-family="monospace" font-weight="bold">E2B_SDK_CLIENT</text>
                <text x="22" y="44" fill="currentColor" fill-opacity="0.4" font-size="7" font-family="monospace">PIN_01: EXEC_CMD</text>
                <text x="22" y="54" fill="currentColor" fill-opacity="0.4" font-size="7" font-family="monospace">PIN_02: FS_IO_STREAM</text>
                <text x="22" y="64" fill="currentColor" fill-opacity="0.4" font-size="7" font-family="monospace">PIN_03: ENV_INJECT</text>

                <!-- Pin Wire Interface Interlock -->
                <line x1="110" y1="40" x2="160" y2="40" stroke="currentColor" stroke-width="1" stroke-opacity="0.7" />
                <line x1="110" y1="50" x2="160" y2="50" stroke="currentColor" stroke-width="1" stroke-opacity="0.7" />
                <circle cx="135" cy="40" r="2" fill="currentColor" />
                <circle cx="135" cy="50" r="2" fill="currentColor" />
                <text x="114" y="32" fill="currentColor" fill-opacity="0.6" font-size="6.5" font-family="monospace">WIRE_SYNC</text>

                <!-- Cube Backend Receiver -->
                <rect x="160" y="16" width="105" height="56" stroke="currentColor" stroke-width="1" stroke-opacity="0.7" fill="currentColor" fill-opacity="0.05" />
                <text x="168" y="30" fill="currentColor" font-size="8" font-family="monospace" font-weight="bold">CUBE_ROUTER_BUS</text>
                <text x="168" y="44" fill="currentColor" fill-opacity="0.5" font-size="7" font-family="monospace">TARGET: LOCAL_DAEMON</text>
                <text x="168" y="54" fill="currentColor" fill-opacity="0.5" font-size="7" font-family="monospace">ADAPTER: PASS_THROUGH</text>
                <text x="168" y="64" fill="currentColor" fill-opacity="0.75" font-size="7" font-family="monospace">OVERHEAD: 0.00ms</text>
              </svg>

              <!-- Schematic 04: eBPF TC Filter & L7 Shield Matrix -->
              <svg v-if="item.id === 'network'" viewBox="0 0 280 88" fill="none" class="schematic-svg">
                <!-- Packet Pipeline -->
                <line x1="15" y1="35" x2="265" y2="35" stroke="currentColor" stroke-width="0.8" stroke-dasharray="2 2" stroke-opacity="0.3" />
                <line x1="15" y1="55" x2="265" y2="55" stroke="currentColor" stroke-width="0.8" stroke-dasharray="2 2" stroke-opacity="0.3" />
                
                <!-- eBPF Hook Gate -->
                <rect x="40" y="18" width="80" height="52" stroke="currentColor" stroke-width="1" stroke-opacity="0.6" fill="currentColor" fill-opacity="0.04" />
                <text x="46" y="30" fill="currentColor" fill-opacity="0.9" font-size="7.5" font-family="monospace" font-weight="bold">eBPF_TC_HOOK</text>
                <text x="46" y="42" fill="currentColor" fill-opacity="0.5" font-size="7" font-family="monospace">EGRESS_POL: PASS</text>
                <text x="46" y="52" fill="currentColor" fill-opacity="0.5" font-size="7" font-family="monospace">INTER_VM: DROP</text>
                <text x="46" y="62" fill="currentColor" fill-opacity="0.4" font-size="6.5" font-family="monospace">PORT: STRICT_MAP</text>

                <!-- Data Packet Flow -->
                <path d="M120 45 H155" stroke="currentColor" stroke-width="1.2" />
                <polygon points="155,42 161,45 155,48" fill="currentColor" />

                <!-- L7 Secret Injection Proxy -->
                <rect x="165" y="18" width="100" height="52" stroke="currentColor" stroke-width="1" stroke-opacity="0.75" fill="currentColor" fill-opacity="0.06" />
                <text x="172" y="30" fill="currentColor" font-size="7.5" font-family="monospace" font-weight="bold">L7_CRED_INJECT</text>
                <text x="172" y="42" fill="currentColor" fill-opacity="0.5" font-size="7" font-family="monospace">AUTH: BEARER_SIGN</text>
                <text x="172" y="52" fill="currentColor" fill-opacity="0.5" font-size="7" font-family="monospace">TOKEN: REDACTED_VM</text>
                <text x="172" y="62" fill="currentColor" fill-opacity="0.75" font-size="6.5" font-family="monospace">LEAK_RISK: ZERO</text>
              </svg>

              <!-- Schematic 05: Silicon Matrix Lattice Density -->
              <svg v-if="item.id === 'density'" viewBox="0 0 280 88" fill="none" class="schematic-svg">
                <!-- Lattice 4x3 MicroVM Density Matrix -->
                <g transform="translate(18, 14)">
                  <!-- 12 Matrix Cores -->
                  <rect x="0" y="0" width="22" height="15" stroke="currentColor" stroke-width="0.8" stroke-opacity="0.8" fill="currentColor" fill-opacity="0.12" />
                  <rect x="28" y="0" width="22" height="15" stroke="currentColor" stroke-width="0.8" stroke-opacity="0.5" />
                  <rect x="56" y="0" width="22" height="15" stroke="currentColor" stroke-width="0.8" stroke-opacity="0.8" fill="currentColor" fill-opacity="0.12" />
                  <rect x="84" y="0" width="22" height="15" stroke="currentColor" stroke-width="0.8" stroke-opacity="0.5" />

                  <rect x="0" y="22" width="22" height="15" stroke="currentColor" stroke-width="0.8" stroke-opacity="0.5" />
                  <rect x="28" y="22" width="22" height="15" stroke="currentColor" stroke-width="0.8" stroke-opacity="0.8" fill="currentColor" fill-opacity="0.12" />
                  <rect x="56" y="22" width="22" height="15" stroke="currentColor" stroke-width="0.8" stroke-opacity="0.5" />
                  <rect x="84" y="22" width="22" height="15" stroke="currentColor" stroke-width="0.8" stroke-opacity="0.8" fill="currentColor" fill-opacity="0.12" />

                  <rect x="0" y="44" width="22" height="15" stroke="currentColor" stroke-width="0.8" stroke-opacity="0.8" fill="currentColor" fill-opacity="0.12" />
                  <rect x="28" y="44" width="22" height="15" stroke="currentColor" stroke-width="0.8" stroke-opacity="0.5" />
                  <rect x="56" y="44" width="22" height="15" stroke="currentColor" stroke-width="0.8" stroke-opacity="0.8" fill="currentColor" fill-opacity="0.12" />
                  <rect x="84" y="44" width="22" height="15" stroke="currentColor" stroke-width="0.8" stroke-opacity="0.5" />
                </g>

                <!-- Bus Coordinator Block -->
                <rect x="145" y="14" width="120" height="59" stroke="currentColor" stroke-width="1" stroke-opacity="0.65" fill="currentColor" fill-opacity="0.04" />
                <text x="153" y="28" fill="currentColor" font-size="7.5" font-family="monospace" font-weight="bold">HOST_MEMORY_BUS</text>
                <text x="153" y="40" fill="currentColor" fill-opacity="0.55" font-size="7" font-family="monospace">OVERHEAD: 3.2MB/VM</text>
                <text x="153" y="50" fill="currentColor" fill-opacity="0.55" font-size="7" font-family="monospace">AUTO_SUSPEND: 500MS</text>
                <text x="153" y="62" fill="currentColor" fill-opacity="0.85" font-size="7.5" font-family="monospace">CAP: 2,048 INST/NODE</text>
              </svg>

              <!-- Schematic 06: State Delta Checkpoint Tree -->
              <svg v-if="item.id === 'state'" viewBox="0 0 220 80" fill="none" class="schematic-svg">
                <!-- Checkpoint Timeline Axis -->
                <line x1="20" y1="40" x2="190" y2="40" stroke="currentColor" stroke-width="0.8" stroke-opacity="0.35" />
                
                <!-- Node S0 -->
                <circle cx="35" cy="40" r="4.5" stroke="currentColor" stroke-width="1" fill="currentColor" fill-opacity="0.15" />
                <text x="30" y="55" fill="currentColor" fill-opacity="0.75" font-size="7.5" font-family="monospace">S0</text>

                <!-- Node S1 (Branching Point) -->
                <circle cx="85" cy="40" r="4.5" stroke="currentColor" stroke-width="1" fill="currentColor" fill-opacity="0.3" />
                <text x="80" y="55" fill="currentColor" fill-opacity="0.75" font-size="7.5" font-family="monospace">S1</text>

                <!-- Fork Upper Branch -->
                <path d="M85 40 Q 115 15, 145 18" stroke="currentColor" stroke-width="1" stroke-opacity="0.75" />
                <circle cx="145" cy="18" r="3.5" fill="currentColor" fill-opacity="0.8" />
                <text x="154" y="21" fill="currentColor" fill-opacity="0.85" font-size="7" font-family="monospace">FORK_A</text>

                <!-- Fork Lower Branch -->
                <path d="M85 40 Q 115 65, 145 62" stroke="currentColor" stroke-width="1" stroke-opacity="0.75" />
                <circle cx="145" cy="62" r="3.5" fill="currentColor" fill-opacity="0.8" />
                <text x="154" y="65" fill="currentColor" fill-opacity="0.85" font-size="7" font-family="monospace">FORK_B</text>

                <!-- Reversible Arc Arrow -->
                <path d="M140 32 Q 110 30, 88 35" stroke="currentColor" stroke-width="0.8" stroke-dasharray="2 1" stroke-opacity="0.6" />
                <polygon points="88,35 94,32 93,37" fill="currentColor" />
                <text x="100" y="28" fill="currentColor" fill-opacity="0.5" font-size="6" font-family="monospace">REVERT</text>
              </svg>

              <!-- Schematic 07: Decoupled Volume Mount Driver -->
              <svg v-if="item.id === 'volume'" viewBox="0 0 220 80" fill="none" class="schematic-svg">
                <!-- Sandbox VM Node -->
                <rect x="15" y="15" width="70" height="50" stroke="currentColor" stroke-width="1" stroke-opacity="0.5" fill="currentColor" fill-opacity="0.03" />
                <text x="22" y="32" fill="currentColor" fill-opacity="0.85" font-size="7.5" font-family="monospace">VM_CONTAINER</text>
                <text x="22" y="44" fill="currentColor" fill-opacity="0.45" font-size="6.5" font-family="monospace">MOUNT: /mnt/vol0</text>
                <text x="22" y="54" fill="currentColor" fill-opacity="0.45" font-size="6.5" font-family="monospace">HOTPLUG: YES</text>

                <!-- Detachable Storage Channel Interlock -->
                <line x1="85" y1="40" x2="130" y2="40" stroke="currentColor" stroke-width="1" stroke-opacity="0.7" />
                <rect x="102" y="36" width="12" height="8" stroke="currentColor" stroke-width="0.8" fill="currentColor" fill-opacity="0.1" />

                <!-- Pluggable Volume Entity -->
                <rect x="130" y="15" width="75" height="50" stroke="currentColor" stroke-width="1" stroke-opacity="0.75" fill="currentColor" fill-opacity="0.06" />
                <text x="138" y="30" fill="currentColor" font-size="7.5" font-family="monospace" font-weight="bold">PERSIST_VOL</text>
                <text x="138" y="42" fill="currentColor" fill-opacity="0.5" font-size="6.5" font-family="monospace">BACKEND: S3/HOST</text>
                <text x="138" y="52" fill="currentColor" fill-opacity="0.5" font-size="6.5" font-family="monospace">LIFECYCLE: INDEP</text>
              </svg>

              <!-- Schematic 08: Distributed High-Availability Topology -->
              <svg v-if="item.id === 'deploy'" viewBox="0 0 220 80" fill="none" class="schematic-svg">
                <!-- Top Controller Node -->
                <rect x="85" y="10" width="50" height="22" stroke="currentColor" stroke-width="1" stroke-opacity="0.8" fill="currentColor" fill-opacity="0.08" />
                <text x="91" y="24" fill="currentColor" font-size="7" font-family="monospace" font-weight="bold">CUBE_MASTER</text>

                <!-- Constellation Network Edges -->
                <line x1="95" y1="32" x2="45" y2="48" stroke="currentColor" stroke-width="0.8" stroke-opacity="0.5" />
                <line x1="125" y1="32" x2="175" y2="48" stroke="currentColor" stroke-width="0.8" stroke-opacity="0.5" />
                <line x1="60" y1="58" x2="160" y2="58" stroke="currentColor" stroke-width="0.8" stroke-dasharray="2 2" stroke-opacity="0.3" />

                <!-- Worker Node 01 -->
                <rect x="18" y="48" width="56" height="22" stroke="currentColor" stroke-width="1" stroke-opacity="0.5" fill="currentColor" fill-opacity="0.03" />
                <text x="21" y="62" fill="currentColor" fill-opacity="0.75" font-size="6.5" font-family="monospace">COMPUTE_01</text>

                <!-- Worker Node 02 -->
                <rect x="146" y="48" width="56" height="22" stroke="currentColor" stroke-width="1" stroke-opacity="0.5" fill="currentColor" fill-opacity="0.03" />
                <text x="149" y="62" fill="currentColor" fill-opacity="0.75" font-size="6.5" font-family="monospace">COMPUTE_02</text>
              </svg>

              <!-- Schematic 09: Native AArch64 Die & Pinout -->
              <svg v-if="item.id === 'arm'" viewBox="0 0 220 80" fill="none" class="schematic-svg">
                <!-- External Die Outline with Pins -->
                <g transform="translate(65, 10)">
                  <!-- Central Die Frame -->
                  <rect x="15" y="12" width="60" height="36" stroke="currentColor" stroke-width="1" stroke-opacity="0.8" fill="currentColor" fill-opacity="0.08" />
                  <text x="22" y="28" fill="currentColor" font-size="8" font-family="monospace" font-weight="bold">ARM64</text>
                  <text x="22" y="38" fill="currentColor" fill-opacity="0.55" font-size="6.5" font-family="monospace">AArch64-v8/v9</text>

                  <!-- Top Pins -->
                  <line x1="25" y1="6" x2="25" y2="12" stroke="currentColor" stroke-width="1" stroke-opacity="0.6" />
                  <line x1="38" y1="6" x2="38" y2="12" stroke="currentColor" stroke-width="1" stroke-opacity="0.6" />
                  <line x1="51" y1="6" x2="51" y2="12" stroke="currentColor" stroke-width="1" stroke-opacity="0.6" />
                  <line x1="64" y1="6" x2="64" y2="12" stroke="currentColor" stroke-width="1" stroke-opacity="0.6" />

                  <!-- Bottom Pins -->
                  <line x1="25" y1="48" x2="25" y2="54" stroke="currentColor" stroke-width="1" stroke-opacity="0.6" />
                  <line x1="38" y1="48" x2="38" y2="54" stroke="currentColor" stroke-width="1" stroke-opacity="0.6" />
                  <line x1="51" y1="48" x2="51" y2="54" stroke="currentColor" stroke-width="1" stroke-opacity="0.6" />
                  <line x1="64" y1="48" x2="64" y2="54" stroke="currentColor" stroke-width="1" stroke-opacity="0.6" />

                  <!-- Left Pins -->
                  <line x1="9" y1="21" x2="15" y2="21" stroke="currentColor" stroke-width="1" stroke-opacity="0.6" />
                  <line x1="9" y1="31" x2="15" y2="31" stroke="currentColor" stroke-width="1" stroke-opacity="0.6" />
                  <line x1="9" y1="39" x2="15" y2="39" stroke="currentColor" stroke-width="1" stroke-opacity="0.6" />

                  <!-- Right Pins -->
                  <line x1="75" y1="21" x2="81" y2="21" stroke="currentColor" stroke-width="1" stroke-opacity="0.6" />
                  <line x1="75" y1="31" x2="81" y2="31" stroke="currentColor" stroke-width="1" stroke-opacity="0.6" />
                  <line x1="75" y1="39" x2="81" y2="39" stroke="currentColor" stroke-width="1" stroke-opacity="0.6" />
                </g>
                <text x="160" y="32" fill="currentColor" fill-opacity="0.5" font-size="6.5" font-family="monospace">SYS_BUS</text>
                <text x="160" y="42" fill="currentColor" fill-opacity="0.8" font-size="6.5" font-family="monospace">NATIVE_PIPELINE</text>
              </svg>
            </div>

            <!-- Content Details -->
            <div class="cell-body">
              <h3 class="cell-title">{{ item.title }}</h3>
              <p class="cell-summary">{{ item.summary }}</p>
            </div>

            <!-- Bottom Document Direct Link -->
            <div class="cell-action">
              <span class="action-text">{{ item.action }}</span>
              <span class="action-arrow" aria-hidden="true">&rarr;</span>
            </div>
          </a>
        </div>
      </div>

    </div>
  </section>
</template>
