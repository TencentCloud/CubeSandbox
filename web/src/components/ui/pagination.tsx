// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useEffect, useRef, useState, type ReactNode, type RefObject } from 'react';
import { ChevronLeft, ChevronRight } from 'lucide-react';
import { cn } from '@/lib/utils';

export type PaginationItem = number | 'ellipsis';

export function pageWindow(page: number, last: number): PaginationItem[] {
  if (last <= 7) {
    return Array.from({ length: last }, (_, i) => i + 1);
  }
  if (page <= 4) {
    return [1, 2, 3, 4, 5, 'ellipsis', last];
  }
  if (page >= last - 3) {
    return [1, 'ellipsis', last - 4, last - 3, last - 2, last - 1, last];
  }
  return [1, 'ellipsis', page - 1, page, page + 1, 'ellipsis', last];
}

type PaginationProps = {
  page: number;
  pageSize: number;
  total: number;
  onPage: (page: number) => void;
  totalLabel: string;
  prevLabel: string;
  nextLabel: string;
  jumpLabel: string;
  jumpUnit?: string;
};

export function Pagination({
  page,
  pageSize,
  total,
  onPage,
  totalLabel,
  prevLabel,
  nextLabel,
  jumpLabel,
  jumpUnit = '',
}: PaginationProps) {
  const jumpRef = useRef<HTMLInputElement>(null);

  if (total <= 0) {
    return null;
  }

  const last = Math.max(1, Math.ceil(total / pageSize));
  const current = Math.min(Math.max(1, page), last);

  return (
    <div className="flex flex-wrap items-center justify-between gap-3 px-4 py-2.5">
      <p className="font-mono text-xs tabular-nums text-foreground/80">{totalLabel}</p>
      {last > 1 ? (
        <div className="flex items-center gap-2">
          <nav className="flex items-center gap-0.5" aria-label="pagination">
            <PagerIconButton
              label={prevLabel}
              disabled={current <= 1}
              onClick={() => onPage(current - 1)}
            >
              <ChevronLeft size={14} />
            </PagerIconButton>
            {pageWindow(current, last).map((item, i) => (
              <PageItem
                key={item === 'ellipsis' ? `e-${i}` : item}
                item={item}
                current={current}
                onPage={onPage}
                jumpRef={jumpRef}
                jumpLabel={jumpLabel}
              />
            ))}
            <PagerIconButton
              label={nextLabel}
              disabled={current >= last}
              onClick={() => onPage(current + 1)}
            >
              <ChevronRight size={14} />
            </PagerIconButton>
          </nav>
          <JumpToPage
            inputRef={jumpRef}
            current={current}
            last={last}
            onPage={onPage}
            jumpLabel={jumpLabel}
            jumpUnit={jumpUnit}
          />
        </div>
      ) : null}
    </div>
  );
}

function parseJump(raw: string, last: number): number | null {
  const n = Number.parseInt(raw, 10);
  if (!Number.isInteger(n)) {
    return null;
  }
  return Math.min(Math.max(1, n), last);
}

function JumpToPage({
  inputRef,
  current,
  last,
  onPage,
  jumpLabel,
  jumpUnit,
}: {
  inputRef: RefObject<HTMLInputElement>;
  current: number;
  last: number;
  onPage: (page: number) => void;
  jumpLabel: string;
  jumpUnit: string;
}) {
  const [draft, setDraft] = useState(String(current));

  useEffect(() => {
    setDraft(String(current));
  }, [current]);

  const commit = () => {
    const next = parseJump(draft, last);
    if (next == null) {
      setDraft(String(current));
      return;
    }
    setDraft(String(next));
    if (next !== current) {
      onPage(next);
    }
  };

  return (
    <label className="flex items-center gap-1.5 text-xs text-muted-foreground">
      <span>{jumpLabel}</span>
      <input
        ref={inputRef}
        type="text"
        inputMode="numeric"
        pattern="[0-9]*"
        aria-label={jumpUnit ? `${jumpLabel} ${jumpUnit}` : jumpLabel}
        value={draft}
        onChange={(e) => setDraft(e.target.value.replace(/\D/g, '').slice(0, 6))}
        onBlur={commit}
        onKeyDown={(e) => {
          if (e.key === 'Enter') {
            e.preventDefault();
            commit();
            e.currentTarget.blur();
          }
          if (e.key === 'Escape') {
            setDraft(String(current));
            e.currentTarget.blur();
          }
        }}
        className={cn(
          'h-7 w-10 rounded-md border border-border/60 bg-transparent px-1 text-center',
          'font-mono text-xs tabular-nums text-foreground',
          'transition-colors hover:bg-muted/40',
          'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-0',
        )}
      />
      {jumpUnit ? <span>{jumpUnit}</span> : null}
    </label>
  );
}

function PageItem({
  item,
  current,
  onPage,
  jumpRef,
  jumpLabel,
}: {
  item: PaginationItem;
  current: number;
  onPage: (page: number) => void;
  jumpRef: RefObject<HTMLInputElement>;
  jumpLabel: string;
}) {
  if (item === 'ellipsis') {
    return (
      <button
        type="button"
        aria-label={jumpLabel}
        onClick={() => {
          jumpRef.current?.focus();
          jumpRef.current?.select();
        }}
        className="flex h-7 w-7 items-center justify-center rounded-md text-xs text-muted-foreground/70 transition-colors hover:bg-muted hover:text-foreground"
      >
        …
      </button>
    );
  }

  const active = item === current;
  return (
    <button
      type="button"
      onClick={() => onPage(item)}
      aria-current={active ? 'page' : undefined}
      className={cn(
        'flex h-7 min-w-7 items-center justify-center rounded-md px-1.5 font-mono text-xs tabular-nums transition-colors',
        active
          ? 'bg-primary text-primary-foreground shadow-sm shadow-primary/20'
          : 'text-muted-foreground hover:bg-muted hover:text-foreground',
      )}
    >
      {item}
    </button>
  );
}

function PagerIconButton({
  label,
  disabled,
  onClick,
  children,
}: {
  label: string;
  disabled: boolean;
  onClick: () => void;
  children: ReactNode;
}) {
  return (
    <button
      type="button"
      aria-label={label}
      disabled={disabled}
      onClick={onClick}
      className={cn(
        'flex h-7 w-7 items-center justify-center rounded-md text-muted-foreground transition-colors',
        disabled ? 'opacity-30 pointer-events-none' : 'hover:bg-muted hover:text-foreground',
      )}
    >
      {children}
    </button>
  );
}
