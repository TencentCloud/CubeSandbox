// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useQuery } from '@tanstack/react-query';
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

// One page holds 500 events; stop after this many pages so a runaway history
// cannot keep the page busy.
const MAX_PAGES = 10;

async function fetchAllEvents(sandboxID: string): Promise<SandboxLifecycleEventDto[]> {
  const events: SandboxLifecycleEventDto[] = [];
  let cursor: number | undefined;
  for (let page = 0; page < MAX_PAGES; page++) {
    const res = await sandboxApi.events(sandboxID, { cursor, limit: 500 });
    events.push(...res.events);
    if (!res.hasMore || res.nextCursor == null) break;
    cursor = res.nextCursor;
  }
  return events;
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
  const history = useQuery({
    queryKey: ['sandbox-events', sandboxID],
    queryFn: () => fetchAllEvents(sandboxID),
    enabled: !!sandboxID,
    retry: 1,
    refetchInterval: live ? 10_000 : false,
  });
  const events = history.data ?? [];

  if (hideWhenEmpty && (history.isLoading || history.isError || events.length === 0)) {
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
      ) : history.isError ? (
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
