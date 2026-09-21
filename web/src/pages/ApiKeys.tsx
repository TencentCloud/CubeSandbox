// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useMemo, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { KeyRound, Plus, Copy, Eye, EyeOff, Trash2, Power, Pencil, Check, X, ShieldCheck } from 'lucide-react';
import { keysApi, type ApiKeyRecord, type GatewayTemplate } from '@/api/keys';
import { Card, CardHeader, CardTitle, CardDescription, CardContent } from '@/components/ui/card';
import { Input } from '@/components/ui/input';
import { Button } from '@/components/ui/button';
import { Badge } from '@/components/ui/badge';
import { Skeleton } from '@/components/ui/skeleton';
import { cn, copyToClipboard } from '@/lib/utils';

function templateLabel(t: GatewayTemplate): string {
  return t.aliases.length > 0 ? t.aliases[0] : t.templateID;
}

export default function ApiKeysPage() {
  const { t } = useTranslation('apiKeys');
  const qc = useQueryClient();
  const [creating, setCreating] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const keys = useQuery({ queryKey: ['gw-keys'], queryFn: () => keysApi.list() });
  const templates = useQuery({ queryKey: ['gw-templates'], queryFn: () => keysApi.templates() });

  const invalidate = () => qc.invalidateQueries({ queryKey: ['gw-keys'] });
  const onError = (e: unknown) => setError(String((e as Error).message ?? e));

  const removeMut = useMutation({ mutationFn: (k: string) => keysApi.remove(k), onSuccess: invalidate, onError });
  const toggleMut = useMutation({
    mutationFn: (rec: ApiKeyRecord) => keysApi.update(rec.key, { enabled: !rec.enabled }),
    onSuccess: invalidate,
    onError,
  });

  return (
    <div className="animate-fade-in space-y-5">
      <header className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">{t('title')}</h1>
          <p className="mt-1 text-sm text-muted-foreground">{t('subtitle')}</p>
        </div>
        <Button onClick={() => setCreating((v) => !v)}>
          <Plus size={14} /> {t('newKey')}
        </Button>
      </header>

      {error && (
        <Card className="!p-3 flex items-center justify-between text-sm text-cube-err">
          <span>{error}</span>
          <button className="text-muted-foreground hover:text-foreground" onClick={() => setError(null)}>
            <X size={14} />
          </button>
        </Card>
      )}

      {creating && (
        <KeyForm
          templates={templates.data ?? []}
          onCancel={() => setCreating(false)}
          onSaved={() => {
            setCreating(false);
            void invalidate();
          }}
          onError={onError}
        />
      )}

      <Card className="!p-0 overflow-hidden">
        <div className="grid grid-cols-[minmax(140px,1fr)_minmax(260px,1.4fr)_minmax(200px,1.6fr)_90px_150px_120px] gap-2 border-b border-border/60 px-4 py-3 text-xs uppercase tracking-wider font-medium text-muted-foreground/85">
          <div>{t('col.name')}</div>
          <div>{t('col.key')}</div>
          <div>{t('col.templates')}</div>
          <div>{t('col.state')}</div>
          <div>{t('col.created')}</div>
          <div className="text-right">{t('col.actions')}</div>
        </div>
        {keys.isLoading &&
          Array.from({ length: 3 }).map((_, i) => (
            <div key={i} className="border-b border-border/60 px-4 py-3">
              <Skeleton className="h-5 w-full" />
            </div>
          ))}
        {(keys.data ?? []).map((rec) => (
          <KeyRow
            key={rec.key}
            rec={rec}
            templates={templates.data ?? []}
            onToggle={() => toggleMut.mutate(rec)}
            onDelete={() => {
              if (window.confirm(t('confirmDelete', { name: rec.name }))) removeMut.mutate(rec.key);
            }}
            onSaved={invalidate}
            onError={onError}
          />
        ))}
        {keys.data && keys.data.length === 0 && (
          <div className="flex flex-col items-center gap-2 py-16 text-sm text-muted-foreground">
            <KeyRound size={28} strokeWidth={1.25} />
            {t('empty')}
          </div>
        )}
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>{t('howto.title')}</CardTitle>
          <CardDescription>{t('howto.desc')}</CardDescription>
        </CardHeader>
        <CardContent>
          <pre className="overflow-auto rounded-md bg-muted/60 p-3 font-mono text-xs leading-relaxed ring-1 ring-border/60">
            {`E2B_API_URL=${window.location.protocol}//${window.location.hostname}:3000\nE2B_SANDBOX_URL=${window.location.protocol}//${window.location.hostname}:3002\nE2B_API_KEY=<key>`}
          </pre>
        </CardContent>
      </Card>
    </div>
  );
}

function TemplatePicker({
  templates,
  value,
  onChange,
}: {
  templates: GatewayTemplate[];
  value: string[];
  onChange: (next: string[]) => void;
}) {
  const { t } = useTranslation('apiKeys');
  const toggle = (name: string) =>
    onChange(value.includes(name) ? value.filter((v) => v !== name) : [...value, name]);
  if (templates.length === 0) return <span className="text-xs text-muted-foreground">{t('form.noTemplates')}</span>;
  return (
    <div className="flex flex-wrap gap-1.5">
      {templates.map((tpl) => {
        const name = templateLabel(tpl);
        const on = value.includes(name) || value.includes(tpl.templateID);
        return (
          <button
            key={tpl.templateID}
            type="button"
            onClick={() => toggle(name)}
            title={`${tpl.templateID} · ${tpl.status}`}
            className={cn(
              'rounded-md border px-2 py-1 text-xs transition-all',
              on
                ? 'border-primary/40 bg-primary/15 text-primary'
                : 'border-border/60 bg-muted/40 text-muted-foreground hover:text-foreground',
              tpl.status !== 'READY' && 'opacity-60',
            )}
          >
            {on && <Check size={11} className="mr-1 inline" />}
            {name}
          </button>
        );
      })}
    </div>
  );
}

function KeyForm({
  templates,
  onCancel,
  onSaved,
  onError,
}: {
  templates: GatewayTemplate[];
  onCancel: () => void;
  onSaved: () => void;
  onError: (e: unknown) => void;
}) {
  const { t } = useTranslation('apiKeys');
  const [name, setName] = useState('');
  const [note, setNote] = useState('');
  const [admin, setAdmin] = useState(false);
  const [picked, setPicked] = useState<string[]>([]);
  const [created, setCreated] = useState<ApiKeyRecord | null>(null);

  const createMut = useMutation({
    mutationFn: () => keysApi.create({ name: name.trim(), templates: picked, admin, note: note.trim() || undefined }),
    onSuccess: (rec) => setCreated(rec),
    onError,
  });

  if (created) {
    return (
      <Card className="border-cube-ok/40">
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <ShieldCheck size={16} className="text-cube-ok" /> {t('form.createdTitle', { name: created.name })}
          </CardTitle>
          <CardDescription>{t('form.createdDesc')}</CardDescription>
        </CardHeader>
        <CardContent className="space-y-3">
          <div className="flex items-center gap-2">
            <code className="flex-1 overflow-auto rounded-md bg-muted/60 px-3 py-2 font-mono text-sm ring-1 ring-border/60">
              {created.key}
            </code>
            <Button variant="outline" onClick={() => copyToClipboard(created.key, t('copied'))}>
              <Copy size={14} /> {t('copy')}
            </Button>
          </div>
          <Button onClick={onSaved}>{t('form.done')}</Button>
        </CardContent>
      </Card>
    );
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t('form.title')}</CardTitle>
        <CardDescription>{t('form.desc')}</CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="grid gap-4 md:grid-cols-2">
          <label className="space-y-1 text-sm">
            <span className="text-xs uppercase tracking-wider text-muted-foreground/85">{t('form.name')}</span>
            <Input value={name} onChange={(e) => setName(e.target.value)} placeholder={t('form.namePlaceholder')} />
          </label>
          <label className="space-y-1 text-sm">
            <span className="text-xs uppercase tracking-wider text-muted-foreground/85">{t('form.note')}</span>
            <Input value={note} onChange={(e) => setNote(e.target.value)} placeholder={t('form.notePlaceholder')} />
          </label>
        </div>
        <div className="space-y-1">
          <span className="text-xs uppercase tracking-wider text-muted-foreground/85">{t('form.templates')}</span>
          <TemplatePicker templates={templates} value={picked} onChange={setPicked} />
          <p className="text-xs text-muted-foreground">{t('form.templatesHint')}</p>
        </div>
        <label className="flex items-center gap-2 text-sm">
          <input type="checkbox" checked={admin} onChange={(e) => setAdmin(e.target.checked)} />
          {t('form.admin')}
          <span className="text-xs text-muted-foreground">— {t('form.adminHint')}</span>
        </label>
        <div className="flex gap-2">
          <Button
            onClick={() => createMut.mutate()}
            disabled={!name.trim() || (!admin && picked.length === 0) || createMut.isPending}
          >
            {t('form.create')}
          </Button>
          <Button variant="ghost" onClick={onCancel}>
            {t('form.cancel')}
          </Button>
        </div>
      </CardContent>
    </Card>
  );
}

function KeyRow({
  rec,
  templates,
  onToggle,
  onDelete,
  onSaved,
  onError,
}: {
  rec: ApiKeyRecord;
  templates: GatewayTemplate[];
  onToggle: () => void;
  onDelete: () => void;
  onSaved: () => void;
  onError: (e: unknown) => void;
}) {
  const { t } = useTranslation('apiKeys');
  const [reveal, setReveal] = useState(false);
  const [editing, setEditing] = useState(false);
  const [picked, setPicked] = useState<string[]>(rec.templates);
  const masked = useMemo(() => `${rec.key.slice(0, 8)}${'•'.repeat(12)}${rec.key.slice(-4)}`, [rec.key]);

  const saveMut = useMutation({
    mutationFn: () => keysApi.update(rec.key, { templates: picked }),
    onSuccess: () => {
      setEditing(false);
      onSaved();
    },
    onError,
  });

  return (
    <div className="border-b border-border/60 px-4 py-2.5 text-sm">
      <div className="grid grid-cols-[minmax(140px,1fr)_minmax(260px,1.4fr)_minmax(200px,1.6fr)_90px_150px_120px] items-center gap-2">
        <div className="min-w-0">
          <div className="flex items-center gap-1.5 truncate font-medium">
            {rec.admin && <ShieldCheck size={13} className="text-primary" />}
            {rec.name}
          </div>
          {rec.note && <div className="truncate text-xs text-muted-foreground">{rec.note}</div>}
        </div>
        <div className="flex min-w-0 items-center gap-1 font-mono text-xs">
          <span className="truncate" title={reveal ? rec.key : undefined}>
            {reveal ? rec.key : masked}
          </span>
          <button className="text-muted-foreground hover:text-foreground" onClick={() => setReveal((v) => !v)} title={t(reveal ? 'hide' : 'reveal')}>
            {reveal ? <EyeOff size={13} /> : <Eye size={13} />}
          </button>
          <button className="text-muted-foreground hover:text-foreground" onClick={() => copyToClipboard(rec.key, t('copied'))} title={t('copy')}>
            <Copy size={13} />
          </button>
        </div>
        <div className="min-w-0">
          {editing ? (
            <TemplatePicker templates={templates} value={picked} onChange={setPicked} />
          ) : rec.admin ? (
            <Badge tone="info">{t('allTemplates')}</Badge>
          ) : rec.templates.length === 0 ? (
            <span className="text-xs text-muted-foreground">{t('noTemplates')}</span>
          ) : (
            <div className="flex flex-wrap gap-1">
              {rec.templates.map((name) => (
                <Link key={name} to={`/audit?template=${encodeURIComponent(name)}`} className="rounded bg-muted/60 px-1.5 py-0.5 text-xs hover:bg-muted">
                  {name}
                </Link>
              ))}
            </div>
          )}
        </div>
        <div>
          <Badge tone={rec.enabled ? 'ok' : 'mute'}>{t(rec.enabled ? 'enabled' : 'disabled')}</Badge>
        </div>
        <div className="text-num text-xs text-muted-foreground">{rec.createdAt ?? '—'}</div>
        <div className="flex items-center justify-end gap-1">
          {editing ? (
            <>
              <Button size="sm" variant="outline" onClick={() => saveMut.mutate()} disabled={saveMut.isPending}>
                <Check size={13} /> {t('save')}
              </Button>
              <Button size="sm" variant="ghost" onClick={() => { setEditing(false); setPicked(rec.templates); }}>
                <X size={13} />
              </Button>
            </>
          ) : (
            <>
              {!rec.admin && (
                <Button size="icon" variant="ghost" onClick={() => setEditing(true)} title={t('editTemplates')}>
                  <Pencil size={14} />
                </Button>
              )}
              <Link to={`/audit?key=${encodeURIComponent(rec.name)}`} title={t('viewAudit')}>
                <Button size="icon" variant="ghost">
                  <KeyRound size={14} />
                </Button>
              </Link>
              <Button size="icon" variant="ghost" onClick={onToggle} title={t(rec.enabled ? 'disable' : 'enable')}>
                <Power size={14} className={cn(!rec.enabled && 'text-cube-warn')} />
              </Button>
              <Button size="icon" variant="ghost" onClick={onDelete} title={t('delete')}>
                <Trash2 size={14} className="text-cube-err" />
              </Button>
            </>
          )}
        </div>
      </div>
    </div>
  );
}
