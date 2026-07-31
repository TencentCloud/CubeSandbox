// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { lazy, Suspense, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { SquareTerminal } from 'lucide-react';

import type { SandboxContainer } from '@/api/client';
import { Button, type ButtonProps } from '@/components/ui/button';
import { terminalAvailable } from '@/lib/terminal';

const TerminalDialog = lazy(() =>
  import('./TerminalDialog').then((module) => ({ default: module.TerminalDialog })),
);

interface Props {
  sandboxID: string;
  state?: string;
  containers?: SandboxContainer[];
  size?: ButtonProps['size'];
  variant?: ButtonProps['variant'];
  iconOnly?: boolean;
}

export function TerminalLauncher({
  sandboxID,
  state,
  containers,
  size = 'default',
  variant = 'outline',
  iconOnly = false,
}: Props) {
  const { t } = useTranslation('terminal');
  const [open, setOpen] = useState(false);
  const sandboxRunning = terminalAvailable(state);
  const available = terminalAvailable(state, containers);
  const unavailableText = sandboxRunning ? t('containerUnavailable') : t('unavailable');

  return (
    <>
      <Button
        size={size}
        variant={variant}
        disabled={!available}
        title={available ? t('open') : unavailableText}
        aria-label={available ? t('open') : unavailableText}
        onClick={() => setOpen(true)}
      >
        <SquareTerminal size={14} />
        {!iconOnly ? t('open') : null}
      </Button>
      {open ? (
        <Suspense fallback={null}>
          <TerminalDialog
            sandboxID={sandboxID}
            containers={containers}
            open={open}
            onOpenChange={setOpen}
          />
        </Suspense>
      ) : null}
    </>
  );
}
