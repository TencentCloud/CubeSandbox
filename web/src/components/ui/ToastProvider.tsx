// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useState, useEffect, useCallback } from 'react';
import { Check, AlertTriangle } from 'lucide-react';
import { cn } from '@/lib/utils';

type ToastType = 'success' | 'warn';

interface ToastItem {
  id: number;
  message: string;
  type: ToastType;
  visible: boolean;
}

let _id = 0;

// 外部调用：window.dispatchEvent(new CustomEvent('cube:toast', { detail: { message: '...', type: 'warn' } }))
export function showToast(message: string, type: ToastType = 'success') {
  window.dispatchEvent(new CustomEvent('cube:toast', { detail: { message, type } }));
}

export function ToastProvider() {
  const [toasts, setToasts] = useState<ToastItem[]>([]);

  const addToast = useCallback((message: string, type: ToastType = 'success') => {
    const id = ++_id;
    setToasts(prev => [...prev, { id, message, type, visible: true }]);
    setTimeout(() => {
      setToasts(prev => prev.map(t => t.id === id ? { ...t, visible: false } : t));
    }, 1400);
    setTimeout(() => {
      setToasts(prev => prev.filter(t => t.id !== id));
    }, 1700);
  }, []);

  useEffect(() => {
    const handler = (e: Event) => {
      const detail = (e as CustomEvent<{ message: string; type?: ToastType }>).detail;
      addToast(detail?.message ?? 'Done', detail?.type ?? 'success');
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
            'transition-all duration-300',
            t.type === 'warn'
              ? 'bg-amber-500 text-white'
              : 'bg-[hsl(var(--foreground))] text-[hsl(var(--background))]',
            t.visible ? 'opacity-100 translate-y-0' : 'opacity-0 translate-y-2',
          )}
        >
          {t.type === 'warn'
            ? <AlertTriangle className="h-3.5 w-3.5 shrink-0" />
            : <Check className="h-3.5 w-3.5 shrink-0" />
          }
          <span>{t.message}</span>
        </div>
      ))}
    </div>
  );
}
