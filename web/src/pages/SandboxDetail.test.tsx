// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { MemoryRouter, Route, Routes } from 'react-router-dom';

import { sandboxApi } from '@/api/client';
import SandboxDetailPage from './SandboxDetail';

const pageMocks = vi.hoisted(() => ({
  get: vi.fn(),
  logs: vi.fn(),
  kill: vi.fn(),
  pause: vi.fn(),
  resume: vi.fn(),
  translate: (key: string, options?: Record<string, unknown>) => {
    const values: Record<string, string> = {
      'actions.terminal': 'Open Terminal',
      'actions.resume': 'Resume',
      'actions.pause': 'Pause',
      'actions.kill': 'Kill',
      'terminal.unavailableLoading': 'Checking terminal availability',
      'terminal.unavailableState': `Terminal unavailable while ${options?.state ?? ''}`,
      'terminal.unavailableEnvd': 'Terminal requires envd',
      'terminal.closeBeforePause': 'Close all terminal tabs before pausing',
      started: `Started ${options?.time ?? ''}`,
      resources: 'Resources',
      runtime: 'Runtime',
      runtimeDesc: 'Runtime details',
      metadata: 'Metadata',
      noMetadata: 'No metadata',
      logs: 'Logs',
      logsDesc: 'Recent logs',
      logsEntries: 'entries',
      logsRefresh: 'Refresh logs',
      logsLoading: 'Loading logs',
      logsEmpty: 'No logs',
      'fields.vcpu': 'vCPU',
      'fields.memory': 'Memory',
      'fields.client': 'Client',
      'fields.alias': 'Alias',
      'fields.started': 'Started',
      'fields.ends': 'Ends',
      'fields.domain': 'Domain',
      'fields.state': 'State',
      'fields.envd': 'envd',
    };
    return values[key] ?? key;
  },
}));

vi.mock('@/api/client', () => ({
  sandboxApi: pageMocks,
}));

vi.mock('@/components/TerminalDialog', () => ({
  TerminalDialog: ({
    open,
    onSessionActiveChange,
  }: {
    open: boolean;
    onSessionActiveChange?: (active: boolean) => void;
  }) => (
    <div data-testid="terminal-dialog">
      {String(open)}
      <button type="button" onClick={() => onSessionActiveChange?.(true)}>
        Mark terminal active
      </button>
      <button type="button" onClick={() => onSessionActiveChange?.(false)}>
        Mark terminal inactive
      </button>
    </div>
  ),
}));

vi.mock('react-i18next', () => ({
  useTranslation: () => ({
    t: pageMocks.translate,
  }),
}));

const sandboxDetail = (overrides: Record<string, unknown> = {}) => ({
  sandboxID: 'sandbox-1',
  templateID: 'template-1',
  clientID: 'node-1',
  state: 'running',
  startedAt: '2026-07-29T00:00:00Z',
  endAt: '2026-07-30T00:00:00Z',
  cpuCount: '2000m',
  memoryMB: 2048,
  envdVersion: '0.2.0',
  metadata: {},
  ...overrides,
});

function renderSandboxDetail(detail?: Record<string, unknown>) {
  const queryClient = new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false },
    },
  });
  if (detail) {
    pageMocks.get.mockResolvedValue(detail);
  }
  pageMocks.logs.mockResolvedValue({ logs: [] });

  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={['/sandboxes/sandbox-1']}>
        <Routes>
          <Route path="/sandboxes/:sandboxID" element={<SandboxDetailPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  pageMocks.get.mockReset();
  pageMocks.logs.mockReset();
  pageMocks.kill.mockReset();
  pageMocks.pause.mockReset();
  pageMocks.resume.mockReset();
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe('SandboxDetail terminal entry', () => {
  it('enables and opens the terminal for a running envd sandbox', async () => {
    renderSandboxDetail(sandboxDetail());
    const button = await screen.findByRole('button', { name: 'Open Terminal' });
    await waitFor(() => expect(button).toBeEnabled());
    fireEvent.click(button);
    await waitFor(() => expect(screen.getByTestId('terminal-dialog')).toHaveTextContent('true'));
  });

  it('keeps the terminal entry visible with a state-specific disabled reason', async () => {
    renderSandboxDetail(sandboxDetail({ state: 'paused' }));
    const button = await screen.findByRole('button', { name: 'Open Terminal' });
    await waitFor(() => {
      expect(button).toBeDisabled();
      expect(button).toHaveAttribute('title', 'Terminal unavailable while paused');
    });
  });

  it('keeps the terminal entry visible when envd is unavailable', async () => {
    renderSandboxDetail(sandboxDetail({ envdVersion: '' }));
    const button = await screen.findByRole('button', { name: 'Open Terminal' });
    await waitFor(() => {
      expect(button).toBeDisabled();
      expect(button).toHaveAttribute('title', 'Terminal requires envd');
    });
  });

  it('shows a disabled terminal entry while details are loading', async () => {
    pageMocks.get.mockReturnValue(new Promise(() => {}));
    renderSandboxDetail();
    const button = await screen.findByRole('button', { name: 'Open Terminal' });
    expect(button).toBeDisabled();
    expect(button).toHaveAttribute('title', 'Checking terminal availability');
  });

  it('blocks pause while any terminal session is active', async () => {
    pageMocks.pause.mockResolvedValue(undefined);
    renderSandboxDetail(sandboxDetail());
    const terminalButton = await screen.findByRole('button', { name: 'Open Terminal' });
    await waitFor(() => expect(terminalButton).toBeEnabled());
    fireEvent.click(terminalButton);
    fireEvent.click(await screen.findByRole('button', { name: 'Mark terminal active' }));

    const pauseButton = screen.getByRole('button', { name: 'Pause' });
    expect(pauseButton).toBeDisabled();
    expect(pauseButton).toHaveAttribute('title', 'Close all terminal tabs before pausing');
    fireEvent.click(pauseButton);
    expect(sandboxApi.pause).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole('button', { name: 'Mark terminal inactive' }));
    await waitFor(() => expect(pauseButton).toBeEnabled());
  });
});
