// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { create } from 'zustand';
import { type Agent } from '@/data/agents';

interface AgentState {
  /** Agents loaded from the backend plus newly created in this session. */
  userAgents: Agent[];
  setAgents: (agents: Agent[]) => void;
  addAgent: (agent: Agent) => void;
  updateAgent: (agent: Agent) => void;
  removeAgent: (id: string) => void;
  /**
   * Configured gateway domain (e.g. "cube.app"). When set, assistants open
   * their OpenClaw gateway via `<port>-<sandboxId>.<domain>` (subdomain origin)
   * instead of the same-origin `/sandbox/` proxy path.
   */
  gatewayDomain: string;
  setGatewayDomain: (domain: string) => void;
}

export const useAgentStore = create<AgentState>()((set) => ({
  userAgents: [],
  gatewayDomain: '',
  setGatewayDomain: (domain) => set({ gatewayDomain: domain }),
  setAgents: (agents) => set({ userAgents: agents }),
  addAgent: (agent) => set((s) => ({ userAgents: [agent, ...s.userAgents] })),
  updateAgent: (agent) =>
    set((s) => ({
      userAgents: s.userAgents.map((item) => (item.id === agent.id ? agent : item)),
    })),
  removeAgent: (id) =>
    set((s) => ({ userAgents: s.userAgents.filter((a) => a.id !== id) })),
}));
