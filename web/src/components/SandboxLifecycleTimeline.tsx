// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { RefreshCw } from 'lucide-react';
import { sandboxApi, type SandboxLifecycleEventDto } from '@/api/client';
import { Card, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { cn } from '@/lib/utils';

const EVENT_TONE: Record<string, 'ok' | 'warn' | 'err' | 'info'> = {
  created: 'ok',
  resumed: 'info',
  paused: 'warn',
  killed: 'err',
};

// One page holds 500 events; one fetch reads at most this many pages so the
// first paint stays quick. The next fetch continues from where this one
// stopped.
const MAX_PAGES = 2;
// How many 5s polls an ended page waits for the kill to be recorded.
const KILL_WAIT_POLLS = 6;
// Only the newest events are kept, so a very long history stays bounded.
const MAX_EVENTS = 2000;

interface History {
  events: SandboxLifecycleEventDto[];
  /** ID of the last event read; the next fetch only asks for newer ones. */
  cursor?: number;
  /** More events exist than were read so far. */
  hasMore: boolean;
  /** Older events were dropped to stay within MAX_EVENTS. */
  trimmed: boolean;
}

async function fetchNewEvents(sandboxID: string, previous?: History): Promise<History> {
  const events = [...(previous?.events ?? [])];
  let cursor = previous?.cursor;
  let hasMore = false;
  for (let page = 0; page < MAX_PAGES; page++) {
    const res = await sandboxApi.events(sandboxID, { cursor, limit: 500 });
    events.push(...res.events);
    if (res.events.length > 0) cursor = res.events[res.events.length - 1].id;
    hasMore = res.hasMore;
    if (!res.hasMore) break;
  }
  // Rows from different CubeMaster replicas can be inserted slightly out of
  // order; the recorded time is authoritative.
  events.sort((a, b) => Date.parse(a.timestamp) - Date.parse(b.timestamp) || a.id - b.id);
  const trimmed = (previous?.trimmed ?? false) || events.length > MAX_EVENTS;
  return { events: events.slice(-MAX_EVENTS), cursor, hasMore, trimmed };
}

/**
 * Lifecycle history of a sandbox (created / paused / resumed / killed). The
 * history outlives the sandbox, so this is also rendered after it ended.
 */
export function SandboxLifecycleTimeline({
  sandboxID,
  live,
  hideWhenEmpty = false,
}: {
  sandboxID: string;
  /** Poll for new events while the sandbox still exists. */
  live: boolean;
  /** Render nothing when no history was recorded (e.g. unknown IDs). */
  hideWhenEmpty?: boolean;
}) {
  const { t } = useTranslation('sandboxDetail');
  const qc = useQueryClient();
  const queryKey = ['sandbox-events', sandboxID];
  const history = useQuery({
    queryKey,
    queryFn: () => fetchNewEvents(sandboxID, qc.getQueryData<History>(queryKey)),
    enabled: !!sandboxID,
    retry: 1,
    refetchInterval: (query) => {
      const data = query.state.data;
      // Keep reading while a long history is still being caught up.
      if (live || data?.hasMore) return 10_000;
      // The kill is recorded just after the sandbox disappears; poll briefly
      // so an ended page that loaded first still gets it.
      const ended = data?.events.some((e) => e.event === 'killed');
      return !ended && query.state.dataUpdateCount < KILL_WAIT_POLLS ? 5_000 : false;
    },
  });
  const events = history.data?.events ?? [];

  if (hideWhenEmpty && events.length === 0) {
    return null;
  }

  return (
    <Card>
      <CardHeader>
        <div className="flex items-center justify-between">
          <div>
            <CardTitle>{t('lifecycle.title')}</CardTitle>
            <CardDescription>{t('lifecycle.description')}</CardDescription>
          </div>
          <Button
            size="icon"
            variant="ghost"
            title={t('logsRefresh')}
            onClick={() => history.refetch()}
            disabled={history.isFetching}
          >
            <RefreshCw size={14} className={cn(history.isFetching && 'animate-spin')} />
          </Button>
        </div>
      </CardHeader>
      {history.isLoading ? (
        <p className="text-sm text-muted-foreground">{t('lifecycle.loading')}</p>
      ) : history.isError && events.length === 0 ? (
        <p className="text-sm text-muted-foreground">{t('lifecycle.error')}</p>
      ) : events.length === 0 ? (
        <p className="text-sm text-muted-foreground">{t('lifecycle.empty')}</p>
      ) : (
        <ol className="space-y-2 text-sm">
          {events.map((event) => (
            <li key={event.id} className="flex items-center gap-3">
              <span className="w-44 shrink-0 font-mono text-xs text-muted-foreground">
                {formatEventTime(event.timestamp)}
              </span>
              <Badge tone={EVENT_TONE[event.event] ?? 'mute'}>
                {t(`lifecycle.events.${event.event}`, { defaultValue: event.event })}
              </Badge>
              <span className="truncate text-xs text-muted-foreground">
                {event.templateID
                  ? t('lifecycle.template', { id: event.templateID })
                  : event.reason
                    ? t(`lifecycle.reasons.${event.reason}`, { defaultValue: event.reason })
                    : null}
              </span>
            </li>
          ))}
        </ol>
      )}
      {history.data?.trimmed ? (
        <p className="mt-2 text-xs text-muted-foreground">
          {t('lifecycle.trimmed', { count: MAX_EVENTS })}
        </p>
      ) : null}
      {history.data?.hasMore ? (
        <p className="mt-2 text-xs text-muted-foreground">{t('lifecycle.loadingMore')}</p>
      ) : null}
    </Card>
  );
}

function formatEventTime(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return new Intl.DateTimeFormat(undefined, {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
  }).format(date);
}
