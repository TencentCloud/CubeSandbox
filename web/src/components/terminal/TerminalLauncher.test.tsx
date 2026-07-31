// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import i18n from '@/i18n';

vi.mock('./TerminalDialog', () => ({
  TerminalDialog: ({ open }: { open: boolean }) => (
    <div data-testid="terminal-dialog" data-open={String(open)} />
  ),
}));

import { TerminalLauncher } from './TerminalLauncher';

describe('TerminalLauncher', () => {
  beforeAll(async () => {
    await i18n.changeLanguage('en');
  });
  afterEach(cleanup);

  it('disables terminal access for a non-running sandbox with a clear hint', () => {
    render(<TerminalLauncher sandboxID="sandbox-1" state="paused" />);
    const button = screen.getByRole('button', {
      name: 'Terminal is available only while the sandbox is running.',
    });
    expect(button).toBeDisabled();
    expect(button).toHaveAttribute(
      'title',
      'Terminal is available only while the sandbox is running.',
    );
    expect(screen.queryByTestId('terminal-dialog')).not.toBeInTheDocument();
  });

  it('opens the terminal dialog for a running sandbox', async () => {
    render(<TerminalLauncher sandboxID="sandbox-1" state="running" />);
    expect(screen.queryByTestId('terminal-dialog')).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Open terminal' }));
    expect(await screen.findByTestId('terminal-dialog')).toHaveAttribute('data-open', 'true');
  });

  it('disables terminal access when detail metadata has no loggable container', () => {
    render(<TerminalLauncher sandboxID="sandbox-1" state="running" containers={[]} />);
    expect(
      screen.getByRole('button', {
        name: 'No running container exposes a terminal endpoint.',
      }),
    ).toBeDisabled();
  });
});
