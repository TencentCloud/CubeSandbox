// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useState } from 'react';
import * as Tooltip from '@radix-ui/react-tooltip';
import { TerminalSquare } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/button';
import { TerminalDialog } from './TerminalDialog';

interface TerminalEntryProps {
  sandboxId: string;
  state?: string | null;
  display?: 'icon' | 'label';
}

export function TerminalEntry({ sandboxId, state, display = 'icon' }: TerminalEntryProps) {
  const { t } = useTranslation('terminal');
  const [open, setOpen] = useState(false);
  const disabled = state !== 'running';
  const tooltip = disabled ? t('entryDisabled') : t('open');

  return (
    <>
      <Tooltip.Provider delayDuration={250}>
        <Tooltip.Root>
          <Tooltip.Trigger asChild>
            <span className="inline-flex" tabIndex={disabled ? 0 : -1}>
              <Button
                type="button"
                size={display === 'icon' ? 'icon' : 'default'}
                variant={display === 'icon' ? 'ghost' : 'outline'}
                aria-label={t('open')}
                title={tooltip}
                disabled={disabled}
                onClick={() => setOpen(true)}
              >
                <TerminalSquare size={display === 'icon' ? 14 : 15} />
                {display === 'label' ? t('open') : null}
              </Button>
            </span>
          </Tooltip.Trigger>
          <Tooltip.Portal>
            <Tooltip.Content
              sideOffset={6}
              className="z-[70] max-w-64 rounded-md border border-border/60 bg-popover px-2.5 py-1.5 text-xs text-popover-foreground shadow-md"
            >
              {tooltip}
              <Tooltip.Arrow className="fill-popover" />
            </Tooltip.Content>
          </Tooltip.Portal>
        </Tooltip.Root>
      </Tooltip.Provider>
      <TerminalDialog open={open} onOpenChange={setOpen} sandboxId={sandboxId} />
    </>
  );
}
