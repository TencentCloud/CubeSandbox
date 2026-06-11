// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { AlertTriangle, X } from 'lucide-react';

export function SandboxActionErrorBanner({
  message,
  onDismiss,
}: {
  message: string | null;
  onDismiss: () => void;
}) {
  if (!message) return null;

  return (
    <div
      role="alert"
      className="flex items-start gap-3 rounded-md border border-cube-amber/40 bg-cube-amber/10 px-4 py-3 text-sm text-cube-amber"
    >
      <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" />
      <p className="flex-1 break-words leading-relaxed">{message}</p>
      <button
        type="button"
        onClick={onDismiss}
        className="shrink-0 rounded p-0.5 text-cube-amber/80 hover:text-cube-amber"
        aria-label="dismiss"
      >
        <X size={14} />
      </button>
    </div>
  );
}
