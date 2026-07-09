// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useState } from 'react';
import { useNavigate, Link } from 'react-router-dom';
import { useQuery, useMutation } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { ArrowLeft, Plus, X } from 'lucide-react';
import { sandboxApi, templateApi } from '@/api/client';
import { Card, CardHeader, CardTitle, CardDescription } from '@/components/ui/card';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Badge } from '@/components/ui/badge';
import { Skeleton } from '@/components/ui/skeleton';
import { cn } from '@/lib/utils';

// ── Types ────────────────────────────────────────────────────────────────────
interface MetaEntry { key: string; value: string }

interface FormState {
  templateID: string;
  timeout: string;
  meta: MetaEntry[];
}

const DEFAULT_FORM: FormState = {
  templateID: '',
  timeout: '',
  meta: [],
};

// ── Template selector card ───────────────────────────────────────────────────
function TemplatePicker({
  selected,
  onSelect,
}: {
  selected: string;
  onSelect: (id: string) => void;
}) {
  const { t } = useTranslation('sandboxNew');
  const { data: templates, isLoading } = useQuery({
    queryKey: ['templates'],
    queryFn: templateApi.list,
    staleTime: 30_000,
  });
  const { data: compat } = useQuery({
    queryKey: ['templates', 'compat'],
    queryFn: templateApi.compat,
    staleTime: 15_000,
  });
  const staleTemplates = new Set(
    (compat?.templates ?? [])
      .filter((row) => row.overall === 'STALE')
      .map((row) => row.templateID),
  );

  if (isLoading) {
    return (
      <div className="grid grid-cols-1 gap-2 sm:grid-cols-2 lg:grid-cols-3">
        {[...Array(3)].map((_, i) => (
          <Skeleton key={i} className="h-20 w-full rounded-lg" />
        ))}
      </div>
    );
  }

  return (
    <div className="grid grid-cols-1 gap-2 sm:grid-cols-2 lg:grid-cols-3">
      {(templates ?? []).map((tpl) => {
        const statusLower = tpl.status.toLowerCase();
        const isReady = statusLower === 'ready';
        const isStale = staleTemplates.has(tpl.templateID);
        const isSelected = tpl.templateID === selected;
        return (
          <button
            key={tpl.templateID}
            type="button"
            disabled={!isReady || isStale}
            onClick={() => onSelect(tpl.templateID)}
            className={cn(
              'flex flex-col gap-1 rounded-lg border px-4 py-3 text-left transition-colors',
              isSelected
                ? 'border-primary bg-primary/10 ring-1 ring-primary'
                : 'border-border bg-card hover:border-primary/50 hover:bg-muted/40',
              (!isReady || isStale) && 'cursor-not-allowed opacity-50',
            )}
          >
            <div className="flex items-center justify-between gap-2">
              <span className="truncate font-mono text-sm font-medium">{tpl.templateID}</span>
              <Badge
                tone={isStale ? 'err' : statusLower === 'ready' ? 'ok' : statusLower === 'building' ? 'warn' : 'err'}
                className="shrink-0 text-xs"
              >
                {isStale ? t('compat.stale') : tpl.status}
              </Badge>
            </div>
            <span className="truncate text-xs text-muted-foreground">
              {tpl.instanceType ?? '—'} · v{tpl.version ?? '—'}
            </span>
          </button>
        );
      })}
    </div>
  );
}

// ── KV metadata editor ───────────────────────────────────────────────────────
function MetaEditor({
  entries,
  onChange,
}: {
  entries: MetaEntry[];
  onChange: (entries: MetaEntry[]) => void;
}) {
  const { t } = useTranslation('sandboxNew');

  const update = (index: number, field: 'key' | 'value', value: string) => {
    const next = entries.map((e, i) => (i === index ? { ...e, [field]: value } : e));
    onChange(next);
  };

  const remove = (index: number) => onChange(entries.filter((_, i) => i !== index));

  const add = () => onChange([...entries, { key: '', value: '' }]);

  return (
    <div className="space-y-2">
      {entries.map((entry, i) => (
        <div key={i} className="flex items-center gap-2">
          <Input
            placeholder={t('form.metaKey')}
            value={entry.key}
            onChange={(e) => update(i, 'key', e.target.value)}
            className="font-mono text-sm"
          />
          <span className="text-muted-foreground">=</span>
          <Input
            placeholder={t('form.metaValue')}
            value={entry.value}
            onChange={(e) => update(i, 'value', e.target.value)}
            className="font-mono text-sm"
          />
          <Button
            type="button"
            variant="ghost"
            size="icon"
            onClick={() => remove(i)}
            className="shrink-0"
          >
            <X size={14} />
          </Button>
        </div>
      ))}
      <Button type="button" variant="outline" size="sm" onClick={add}>
        <Plus size={14} /> {t('form.addMeta')}
      </Button>
    </div>
  );
}

// ── Section wrapper ──────────────────────────────────────────────────────────
function Section({ title, description, children }: { title: string; description?: string; children: React.ReactNode }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>{title}</CardTitle>
        {description && <CardDescription>{description}</CardDescription>}
      </CardHeader>
      <div className="space-y-4">{children}</div>
    </Card>
  );
}

// ── Main page ────────────────────────────────────────────────────────────────
export default function SandboxNewPage() {
  const nav = useNavigate();
  const { t } = useTranslation('sandboxNew');
  const [form, setForm] = useState<FormState>(DEFAULT_FORM);
  const [error, setError] = useState<string | null>(null);
  const { data: compat } = useQuery({
    queryKey: ['templates', 'compat'],
    queryFn: templateApi.compat,
    staleTime: 15_000,
  });

  const set = <K extends keyof FormState>(key: K, value: FormState[K]) =>
    setForm((prev) => ({ ...prev, [key]: value }));

  const create = useMutation({
    mutationFn: () => {
      const metadata: Record<string, string> = {};
      form.meta.forEach(({ key, value }) => {
        if (key.trim()) metadata[key.trim()] = value;
      });
      const parsedTimeout = Number.parseInt(form.timeout, 10);
      return sandboxApi.create({
        templateID: form.templateID,
        timeout: Number.isFinite(parsedTimeout) && parsedTimeout >= 0 ? parsedTimeout : undefined,
        metadata: Object.keys(metadata).length > 0 ? metadata : undefined,
      });
    },
    onSuccess: (sandbox) => {
      nav(`/sandboxes/${sandbox.sandboxID}`);
    },
    onError: (err: Error) => {
      setError(err.message);
    },
  });

  const selectedCompat = compat?.templates.find((row) => row.templateID === form.templateID);
  const selectedTemplateStale = selectedCompat?.overall === 'STALE';
  const canSubmit = !!form.templateID && !selectedTemplateStale && !create.isPending;

  return (
    <div className="animate-fade-in space-y-5">
      {/* Header */}
      <div className="flex items-center gap-3">
        <Link to="/sandboxes">
          <Button variant="ghost" size="icon">
            <ArrowLeft size={16} />
          </Button>
        </Link>
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">{t('title')}</h1>
          <p className="mt-0.5 text-sm text-muted-foreground">{t('subtitle')}</p>
        </div>
      </div>

      {/* Template */}
      <Section title={t('section.template')} description={t('section.templateDesc')}>
        <TemplatePicker selected={form.templateID} onSelect={(id) => set('templateID', id)} />
        {!form.templateID && (
          <p className="text-xs text-muted-foreground">{t('form.templateRequired')}</p>
        )}
        {selectedTemplateStale && (
          <p className="text-xs text-destructive">{t('compat.staleHelp')}</p>
        )}
      </Section>

      {/* Timeout */}
      <Section title={t('section.config')} description={t('section.configDesc')}>
        <div className="space-y-1.5">
          <label className="text-sm font-medium text-foreground">{t('form.timeout')}</label>
          <Input
            type="number"
            min={0}
            step={60}
            placeholder={t('form.timeoutPlaceholder')}
            value={form.timeout}
            onChange={(e) => set('timeout', e.target.value)}
            className="font-mono text-sm"
          />
          <p className="text-xs text-muted-foreground">{t('form.timeoutHint')}</p>
        </div>
      </Section>

      {/* Metadata */}
      <Section title={t('section.metadata')} description={t('section.metadataDesc')}>
        <MetaEditor entries={form.meta} onChange={(meta) => set('meta', meta)} />
      </Section>

      {/* Error */}
      {error && (
        <div className="rounded-md border border-cube-err/40 bg-cube-err/10 px-4 py-3 text-sm text-cube-err">
          {error}
        </div>
      )}

      {/* Submit */}
      <div className="flex items-center justify-end gap-3 pb-6">
        <Link to="/sandboxes">
          <Button variant="outline">{t('actions.cancel')}</Button>
        </Link>
        <Button onClick={() => create.mutate()} disabled={!canSubmit}>
          {create.isPending ? t('actions.creating') : t('actions.create')}
        </Button>
      </div>
    </div>
  );
}
