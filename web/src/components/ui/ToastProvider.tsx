// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useState, useEffect, useCallback } from 'react';
import { Check } from 'lucide-react';
import { cn } from '@/lib/utils';

interface ToastItem {
  id: number;
  message: string;
  visible: boolean;
}

let _id = 0;

export function ToastProvider() {
  const [toasts, setToasts] = useState<ToastItem[]>([]);

  const addToast = useCallback((message: string) => {
    const id = ++_id;
    setToasts(prev => [...prev, { id, message, visible: true }]);
    // start fade-out after 1.4s, remove after 1.7s
    setTimeout(() => {
      setToasts(prev => prev.map(t => t.id === id ? { ...t, visible: false } : t));
    }, 1400);
    setTimeout(() => {
      setToasts(prev => prev.filter(t => t.id !== id));
    }, 1700);
  }, []);

  useEffect(() => {
    const handler = (e: Event) => {
      const detail = (e as CustomEvent<{ message: string }>).detail;
      addToast(detail?.message ?? '已复制');
    };
    window.addEventListener('cube:toast', handler);
    return () => window.removeEventListener('cube:toast', handler);
  }, [addToast]);

  if (toasts.length === 0) return null;

  return (
    <div className="fixed bottom-6 left-1/2 -translate-x-1/2 z-[9999] flex flex-col items-center gap-2 pointer-events-none">
      {toasts.map(t => (
        <div
          key={t.id}
          className={cn(
            'flex items-center gap-2 rounded-full px-4 py-2 text-sm font-medium shadow-lg',
            'bg-[hsl(var(--foreground))] text-[hsl(var(--background))]',
            'transition-all duration-300',
            t.visible ? 'opacity-100 translate-y-0' : 'opacity-0 translate-y-2',
          )}
        >
          <Check className="h-3.5 w-3.5 shrink-0" />
          <span>{t.message}</span>
        </div>
      ))}
    </div>
  );
}
