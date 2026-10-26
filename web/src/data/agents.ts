// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

export type AgentEngine = 'openclaw' | 'hermes';
export type AgentEnv = 'linux' | 'mac';
export type AgentStatus = 'running' | 'starting' | 'stopped' | 'error';
export type RobotChannel = 'wecom';
export type AgentPersistenceMode = 'full_snapshot' | 'shared_files';

export interface Agent {
  id: string;
  name: string;
  status: AgentStatus;
  engine: AgentEngine;
  env: AgentEnv;
  model: string;
  version: string;
  /** Robot/IM channels already bound (rendered as filled chips). */
  bots: RobotChannel[];
  /** Channels still available to add (rendered as `+ XXX` outline chips). */
  botsAvailable: RobotChannel[];
  /** Avatar emoji or short text — replaced by illustrations later. */
  avatar: string;
  /** Color hint for avatar background (tailwind class fragment). */
  avatarTone: 'sky' | 'amber' | 'emerald' | 'rose' | 'violet';
  sandboxId?: string;
  templateId?: string;
  gatewayUrl?: string;
  envUrl?: string;
  persistenceMode?: AgentPersistenceMode;
  rootfsSourceType?: 'template' | 'snapshot';
  rootfsSourceId?: string;
  openclawPersistId?: string;
  openclawStatePath?: string;
  wecomConfig?: {
    botId: string;
    botSecret: string;
  };
}

export const ROBOT_CHANNELS: Record<RobotChannel, { label: string }> = {
  wecom: { label: 'WeCom' },
};
