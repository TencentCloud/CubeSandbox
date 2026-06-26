// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.
//
// Topology graph for the SandboxCases page. Renders the management + data
// flow using @xyflow/react, with a custom node component that picks its
// color from the `plane` field.
//
// The layout is deterministic: nodes are placed on a fixed grid that walks
// the topology left-to-right, with control-plane nodes on top and
// data-plane nodes on the bottom half. This keeps the bundle small (no
// dagre / elk dependency) and the result easy to scan.

import {
  Background,
  Controls,
  Handle,
  MiniMap,
  Position,
  ReactFlow,
  ReactFlowProvider,
  type Edge,
  type Node,
  type NodeProps,
  type NodeTypes,
} from '@xyflow/react';
import '@xyflow/react/dist/style.css';

import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Maximize2, Minimize2, Network } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { cn } from '@/lib/utils';
import type { Plane, ScenarioEdge, ScenarioNode } from '@/data/exampleScenarios';

export interface TopologyGraphProps {
  nodes: ScenarioNode[];
  edges: ScenarioEdge[];
  className?: string;
  /** Optional fixed height in pixels. Default: 360. */
  height?: number;
  /** Show a compact legend under the canvas. Default: true. */
  showLegend?: boolean;
}

const COL_WIDTH = 250;
const ROW_HEIGHT_CONTROL = 120;
const ROW_HEIGHT_DATA = 120;
const COL_GAP = 60;

// Layer ordering used by the layout. Each entry is a list of node ids that
// share the same x-coordinate. Unknown nodes are appended at the end in
// the order they appear in `nodes`.
function topoLayeredOrder(nodes: ScenarioNode[]): string[][] {
  // Layout layers (left-to-right). Nodes in the same layer share the same
  // x-coordinate. Control-plane nodes are rendered on the top row, data-plane
  // nodes on the bottom row within each layer.
  const knownLayers: Record<string, number> = {
    // Layer 0: Entry point
    user: 0,
    // Layer 1: Gateway
    cubeapi: 1,
    sidecar: 1,
    // Layer 2: Orchestration
    cubemaster: 2,
    // Layer 3: Node agent + data-plane entry (max 2 per column)
    cubelet: 3,
    cubeproxy: 3,
    // Layer 4: Runtime shim + Network agent (max 2 per column)
    cubeshim: 4,
    'network-agent': 4,
    // Layer 5: Hypervisor
    'cube-hypervisor': 5,
    // Layer 6: Sandbox boundary (MicroVM) + external stores
    microvm: 6,
    'microvm-0': 6,
    'microvm-1': 6,
    'microvm-2': 6,
    'microvm-3': 6,
    hostdir: 6,
    snapshot: 6,
    // Layer 7: Sandbox runtime
    'cube-runtime': 7,
    // Layer 8: Workload
    playwright: 8,
    nginx: 8,
    chromium: 8,
  };
  const seen = new Set<string>();
  const layers: string[][] = [];
  for (const n of nodes) {
    if (seen.has(n.id)) continue;
    seen.add(n.id);
    const layerIdx = knownLayers[n.id] ?? layers.length;
    while (layers.length <= layerIdx) layers.push([]);
    layers[layerIdx].push(n.id);
  }
  return layers;
}

function layoutNodes(nodes: LocalizedNode[]): Node<TopologyNodeData>[] {
  const nodeMap = new Map<string, LocalizedNode>(nodes.map((n) => [n.id, n]));
  const layers = topoLayeredOrder(nodes);
  const out: Node<TopologyNodeData>[] = [];
  for (let col = 0; col < layers.length; col++) {
    const layer = layers[col];
    const controlInLayer = layer.filter((id) => nodeMap.get(id)?.plane === 'control');
    const dataInLayer = layer.filter((id) => nodeMap.get(id)?.plane === 'data');
    let controlIdx = 0;
    let dataIdx = 0;
    for (const id of layer) {
      const node = nodeMap.get(id);
      if (!node) continue;
      const x = col * (COL_WIDTH + COL_GAP);
      if (node.plane === 'control') {
        const total = controlInLayer.length;
        const offsetY = ((controlIdx - (total - 1) / 2) * ROW_HEIGHT_CONTROL);
        controlIdx++;
        out.push({
          id: node.id,
          type: 'topologyNode',
          data: { node },
          position: { x, y: 80 + offsetY },
          sourcePosition: Position.Right,
          targetPosition: Position.Left,
          draggable: true,
        });
      } else {
        const total = dataInLayer.length;
        const offsetY = ((dataIdx - (total - 1) / 2) * ROW_HEIGHT_DATA);
        dataIdx++;
        out.push({
          id: node.id,
          type: 'topologyNode',
          data: { node },
          position: { x, y: 320 + offsetY },
          sourcePosition: Position.Right,
          targetPosition: Position.Left,
          draggable: true,
        });
      }
    }
  }
  return out;
}

function buildEdges(edges: LocalizedEdge[]): Edge[] {
  return edges.map((e, idx) => ({
    id: `e-${e.from}-${e.to}-${idx}`,
    source: e.from,
    target: e.to,
    label: e.label,
    type: 'smoothstep',
    animated: e.plane === 'data',
    style: {
      stroke: e.plane === 'control' ? '#22d3ee' : '#a78bfa',
      strokeOpacity: 0.55,
      strokeWidth: 1.5,
    },
    labelStyle: { fill: '#a1a1aa', fontSize: 10, fontWeight: 500 },
    labelBgStyle: { fill: '#0b0d12', fillOpacity: 0.85 },
    labelBgPadding: [4, 2],
    labelBgBorderRadius: 4,
  }));
}

interface LocalizedNode extends ScenarioNode {
  label: string;
  description: string;
}

interface LocalizedEdge extends ScenarioEdge {
  label: string;
}

interface TopologyNodeData extends Record<string, unknown> {
  node: LocalizedNode;
}

function planeColor(plane: Plane): { ring: string; bg: string; text: string; pill: string } {
  if (plane === 'control') {
    return {
      ring: 'ring-cube-cyan/50',
      bg: 'bg-cube-cyan/10',
      text: 'text-cube-cyan',
      pill: 'bg-cube-cyan/15 text-cube-cyan ring-cube-cyan/30',
    };
  }
  return {
    ring: 'ring-cube-violet/50',
    bg: 'bg-cube-violet/10',
    text: 'text-cube-violet',
    pill: 'bg-cube-violet/15 text-cube-violet ring-cube-violet/30',
  };
}

function TopologyNodeView({ data, selected }: NodeProps<Node<TopologyNodeData>>) {
  const { node } = data;
  const tone = planeColor(node.plane);
  return (
    <div
      className={cn(
        'group relative flex w-[220px] flex-col gap-1.5 rounded-lg border border-border/60 bg-card/80 px-3 py-2.5 shadow-sm backdrop-blur-md transition-all',
        tone.ring,
        'ring-1',
        selected && 'shadow-md ring-2',
      )}
    >
      <Handle type="target" position={Position.Left} className="!h-2 !w-2 !bg-muted-foreground/60" />
      <div className="flex items-center justify-between gap-1">
        <span className={cn('rounded px-1.5 py-0.5 text-[9px] font-semibold uppercase tracking-wider ring-1', tone.pill)}>
          {node.plane === 'control' ? 'ctrl' : 'data'}
        </span>
        <span className="text-[9px] font-medium uppercase tracking-wider text-muted-foreground/60">
          {node.kind}
        </span>
      </div>
      <p className={cn('truncate text-[12.5px] font-semibold leading-tight', tone.text)}>{node.label}</p>
      <p className="line-clamp-2 text-[10.5px] leading-snug text-muted-foreground/80">{node.description}</p>
      <Handle type="source" position={Position.Right} className="!h-2 !w-2 !bg-muted-foreground/60" />
    </div>
  );
}

const NODE_TYPES: NodeTypes = { topologyNode: TopologyNodeView };

function TopologyGraphInner({ nodes, edges, className, height = 460, showLegend = true }: TopologyGraphProps) {
  const { t, i18n } = useTranslation('examples');
  const [expanded, setExpanded] = useState(false);
  const isZh = (i18n.language ?? 'en').toLowerCase().startsWith('zh');

  // Translate labels / descriptions on the fly using the catalogue.
  const localizedNodes = useMemo(
    () =>
      nodes.map((n) => ({
        ...n,
        label: isZh ? n.labelZh : n.labelEn,
        description: isZh ? n.descriptionZh : n.descriptionEn,
      })),
    [nodes, isZh],
  );
  const localizedEdges = useMemo(
    () => edges.map((e) => ({ ...e, label: isZh ? e.labelZh : e.labelEn })),
    [edges, isZh],
  );

  const flowNodes = useMemo(() => layoutNodes(localizedNodes), [localizedNodes]);
  const flowEdges = useMemo(() => buildEdges(localizedEdges), [localizedEdges]);

  return (
    <div
      className={cn(
        'relative overflow-hidden rounded-lg border border-border/60 bg-[#0b0d12]/80',
        className,
      )}
      style={{ height: expanded ? 700 : height }}
    >
      {/* Plane labels on the canvas sides */}
      <div className="pointer-events-none absolute left-3 top-3 z-10 flex flex-col gap-1.5 text-[10px] font-semibold uppercase tracking-wider">
        <span className="rounded bg-cube-cyan/15 px-2 py-0.5 text-cube-cyan ring-1 ring-cube-cyan/30">
          {t('topology.controlPlane')}
        </span>
        <span className="rounded bg-cube-violet/15 px-2 py-0.5 text-cube-violet ring-1 ring-cube-violet/30">
          {t('topology.dataPlane')}
        </span>
      </div>

      <div className="absolute right-3 top-3 z-10 flex items-center gap-1.5">
        <Button
          size="icon"
          variant="ghost"
          className="h-7 w-7 bg-background/60 backdrop-blur"
          title={expanded ? 'Collapse' : 'Expand'}
          onClick={() => setExpanded((e) => !e)}
        >
          {expanded ? <Minimize2 size={13} /> : <Maximize2 size={13} />}
        </Button>
      </div>

      <ReactFlow
        nodes={flowNodes}
        edges={flowEdges}
        nodeTypes={NODE_TYPES}
        fitView
        fitViewOptions={{ padding: 0.18 }}
        minZoom={0.4}
        maxZoom={1.6}
        panOnScroll
        zoomOnDoubleClick={false}
        proOptions={{ hideAttribution: true }}
        nodesDraggable
        elementsSelectable
        panActivationKeyCode={null}
      >
        <Background gap={20} size={1} color="#1f2937" />

        <Controls
          showInteractive={false}
          className="!bg-transparent !backdrop-blur-none !border-none !shadow-none [&>button]:!h-6 [&>button]:!w-6 [&>button]:!bg-card/40 [&>button]:!backdrop-blur [&>button]:!border-border/40 [&>button]:!text-muted-foreground hover:[&>button]:!bg-card/70 hover:[&>button]:!text-foreground [&>button>svg]:!h-3 [&>button>svg]:!w-3"
          position="bottom-right"
        />
      </ReactFlow>

      {showLegend && (
        <div className="absolute bottom-2 left-3 z-10 flex items-center gap-3 rounded-md bg-background/70 px-2.5 py-1 text-[10px] text-muted-foreground backdrop-blur">
          <span className="inline-flex items-center gap-1">
            <span className="h-1.5 w-3 rounded-full bg-cube-cyan" />
            {t('topology.controlPlane')}
          </span>
          <span className="inline-flex items-center gap-1">
            <span className="h-1.5 w-3 rounded-full bg-cube-violet" />
            {t('topology.dataPlane')}
          </span>
          <span className="inline-flex items-center gap-1">
            <Network size={11} />
            {nodes.length} nodes · {edges.length} edges
          </span>
        </div>
      )}
    </div>
  );
}

export function TopologyGraph(props: TopologyGraphProps) {
  return (
    <ReactFlowProvider>
      <TopologyGraphInner {...props} />
    </ReactFlowProvider>
  );
}