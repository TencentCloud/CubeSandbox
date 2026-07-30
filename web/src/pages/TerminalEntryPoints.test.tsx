// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { sandboxApi } from '@/api/client';
import SandboxesPage from './Sandboxes';
import SandboxDetailPage from './SandboxDetail';

vi.mock('@/components/terminal/TerminalEntry', () => ({
  TerminalEntry: ({ sandboxId, state, display = 'icon' }: Record<string, string>) => (
    <button
      type="button"
      aria-label={`terminal-${sandboxId}`}
      data-display={display}
      disabled={state !== 'running'}
    />
  ),
}));

afterEach(() => vi.restoreAllMocks());

describe('terminal page entry points', () => {
  it('wires list row actions to exact running state only', async () => {
    vi.spyOn(sandboxApi, 'list').mockResolvedValue([
      sandbox('sandbox-running', 'running'),
      sandbox('sandbox-paused', 'paused'),
      sandbox('sandbox-pausing', 'pausing'),
    ] as never);
    renderWithClient(
      <MemoryRouter initialEntries={['/sandboxes']}>
        <SandboxesPage />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByLabelText('terminal-sandbox-running')).toBeEnabled());
    expect(screen.getByLabelText('terminal-sandbox-paused')).toBeDisabled();
    expect(screen.getByLabelText('terminal-sandbox-pausing')).toBeDisabled();
    expect(screen.getByLabelText('terminal-sandbox-running')).toHaveAttribute(
      'data-display',
      'icon',
    );
  });

  it('wires the detail header to an icon-and-text running-only action', async () => {
    vi.spyOn(sandboxApi, 'get').mockResolvedValue(sandbox('sandbox-running', 'running') as never);
    vi.spyOn(sandboxApi, 'logs').mockResolvedValue({ logs: [] } as never);
    renderWithClient(
      <MemoryRouter initialEntries={['/sandboxes/sandbox-running']}>
        <Routes>
          <Route path="/sandboxes/:sandboxID" element={<SandboxDetailPage />} />
        </Routes>
      </MemoryRouter>,
    );

    const entry = await screen.findByLabelText('terminal-sandbox-running');
    expect(entry).toBeEnabled();
    expect(entry).toHaveAttribute('data-display', 'label');
  });

  it('disables the detail header entry when the sandbox is paused', async () => {
    vi.spyOn(sandboxApi, 'get').mockResolvedValue(sandbox('sandbox-paused', 'paused') as never);
    vi.spyOn(sandboxApi, 'logs').mockResolvedValue({ logs: [] } as never);
    renderWithClient(
      <MemoryRouter initialEntries={['/sandboxes/sandbox-paused']}>
        <Routes>
          <Route path="/sandboxes/:sandboxID" element={<SandboxDetailPage />} />
        </Routes>
      </MemoryRouter>,
    );

    expect(await screen.findByLabelText('terminal-sandbox-paused')).toBeDisabled();
  });
});

function renderWithClient(children: React.ReactNode) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } },
  });
  return render(<QueryClientProvider client={client}>{children}</QueryClientProvider>);
}

function sandbox(sandboxID: string, state: string) {
  return {
    sandboxID,
    state,
    templateID: 'template-a',
    cpuCount: 2,
    memoryMB: 1024,
    startedAt: '2026-07-30T10:00:00Z',
    endAt: '2026-07-30T11:00:00Z',
    metadata: {},
  };
}
