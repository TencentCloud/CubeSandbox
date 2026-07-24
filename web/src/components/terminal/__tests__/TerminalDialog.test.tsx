// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { ReactNode } from 'react';

import type { SandboxDetail } from '@/api/client';
import { sandboxApi } from '@/api/client';
import { useTerminal } from '../useTerminal';
import { TerminalDialog } from '../TerminalDialog';

// Controllable hook state; tests mutate `hookState` before rendering.
const { hookState } = vi.hoisted(() => ({
  hookState: {
    status: 'ready' as string,
    exitCode: null as number | null,
    errorMessage: null as string | null,
    reconnect: vi.fn(),
  },
}));

vi.mock('../useTerminal', () => ({
  useTerminal: vi.fn(() => ({
    containerRef: vi.fn(),
    status: hookState.status,
    exitCode: hookState.exitCode,
    errorMessage: hookState.errorMessage,
    reconnect: hookState.reconnect,
    fontSize: 13,
    increaseFontSize: vi.fn(),
    decreaseFontSize: vi.fn(),
  })),
}));

vi.mock('@/api/client', () => ({
  sandboxApi: { get: vi.fn() },
}));

// Avoid loading the full i18n setup; keys render verbatim.
vi.mock('react-i18next', () => ({
  useTranslation: () => ({ t: (key: string) => key }),
}));

function detail(containers: SandboxDetail['containers']): SandboxDetail {
  return { sandboxID: 'sb-1', containers } as SandboxDetail;
}

function renderDialog() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  return render(<TerminalDialog sandboxID="sb-1" open onOpenChange={() => {}} />, { wrapper });
}

const mockedUseTerminal = vi.mocked(useTerminal);
const mockedGet = vi.mocked(sandboxApi.get);

beforeEach(() => {
  vi.clearAllMocks();
  hookState.status = 'ready';
  hookState.exitCode = null;
  hookState.errorMessage = null;
  mockedGet.mockResolvedValue(detail(null));
});

describe('TerminalDialog', () => {
  it('shows the ready badge when the terminal is connected', async () => {
    renderDialog();
    expect(await screen.findByText('status.ready')).toBeInTheDocument();
  });

  it('shows an overlay with a reconnect button when not ready', async () => {
    hookState.status = 'error';
    hookState.errorMessage = 'boom';
    renderDialog();
    const button = await screen.findByRole('button', { name: /reconnect/ });
    // The message appears both in the header badge and the overlay.
    expect(screen.getAllByText('boom').length).toBeGreaterThan(0);
    fireEvent.click(button);
    expect(hookState.reconnect).toHaveBeenCalled();
  });

  it('renders a container selector for multi-container sandboxes and switches containers', async () => {
    mockedGet.mockResolvedValue(
      detail([
        { name: 'sidecar', containerID: 'ctr-side', kind: 'sidecar' },
        { name: 'main', containerID: 'ctr-main', kind: 'sandbox', envdPort: 49983 },
      ]),
    );
    renderDialog();

    const select = (await screen.findByLabelText('selectContainer')) as HTMLSelectElement;
    // Defaults to the primary container (kind === 'sandbox').
    expect(select.value).toBe('ctr-main');
    await waitFor(() =>
      expect(mockedUseTerminal).toHaveBeenLastCalledWith('sb-1', true, 'ctr-main'),
    );

    fireEvent.change(select, { target: { value: 'ctr-side' } });
    await waitFor(() =>
      expect(mockedUseTerminal).toHaveBeenLastCalledWith('sb-1', true, 'ctr-side'),
    );
  });

  it('hides the selector for single-container sandboxes', async () => {
    mockedGet.mockResolvedValue(
      detail([{ name: 'main', containerID: 'ctr-main', kind: 'sandbox' }]),
    );
    renderDialog();
    await screen.findByText('status.ready');
    await waitFor(() => expect(mockedGet).toHaveBeenCalled());
    expect(screen.queryByLabelText('selectContainer')).toBeNull();
    expect(mockedUseTerminal).toHaveBeenLastCalledWith('sb-1', true, undefined);
  });

  it('hides the selector when the detail carries no container list', async () => {
    renderDialog();
    await screen.findByText('status.ready');
    await waitFor(() => expect(mockedGet).toHaveBeenCalled());
    expect(screen.queryByLabelText('selectContainer')).toBeNull();
  });
});
