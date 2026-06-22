// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useTranslation } from 'react-i18next';
import { useState } from 'react';
import { useLocation, useNavigate } from 'react-router-dom';
import { useQuery } from '@tanstack/react-query';
import { useRuntimeConfig } from '@/hooks/useRuntimeConfig';
import {
  Palette, Plug, Keyboard, Info,
  Sun, Moon, Monitor, Check, ExternalLink,
  Loader2, Wifi, WifiOff, UserCog, LogOut, KeyRound,
} from 'lucide-react';
import { useThemeStore, type ThemeMode } from '@/store/theme';
import { clusterApi, authApi } from '@/api/client';
import { useControlPlaneVersion } from '@/hooks/useControlPlaneVersion';
import { ApiError } from '@/lib/api';
import { clearSession, getSessionUser } from '@/lib/session';
import { cn } from '@/lib/utils';

// ── Sidebar nav ───────────────────────────────────────────────────────────────

const SECTIONS = [
  { key: 'appearance', icon: Palette },
  { key: 'cluster',    icon: Plug },
  { key: 'account',    icon: UserCog },
  { key: 'shortcuts',  icon: Keyboard },
  { key: 'about',      icon: Info },
] as const;

function SettingsSidebar({ active, onChange }: {
  active: string;
  onChange: (k: string) => void;
}) {
  const { t } = useTranslation('settings');
  return (
    <nav className="w-44 shrink-0 space-y-0.5">
      {SECTIONS.map(({ key, icon: Icon }) => (
        <button
          key={key}
          onClick={() => onChange(key)}
          className={cn(
            'flex w-full items-center gap-2.5 rounded-lg px-3 py-2 text-sm transition-colors',
            active === key
              ? 'bg-primary/10 text-primary font-medium'
              : 'text-muted-foreground hover:bg-muted/50 hover:text-foreground'
          )}
        >
          <Icon size={15} />
          {t(`nav.${key}`)}
        </button>
      ))}
    </nav>
  );
}

// ── Section: Appearance ───────────────────────────────────────────────────────

const THEME_OPTIONS: { value: ThemeMode; icon: typeof Sun; labelKey: string }[] = [
  { value: 'system', icon: Monitor, labelKey: 'system' },
  { value: 'light',  icon: Sun,     labelKey: 'light' },
  { value: 'dark',   icon: Moon,    labelKey: 'dark' },
];

const LANGS = [
  { code: 'zh', label: '简体中文' },
  { code: 'en', label: 'English' },
] as const;

function AppearanceSection() {
  const { t } = useTranslation('settings');
  const { t: tTheme } = useTranslation('theme');
  const { i18n } = useTranslation();
  const mode = useThemeStore((s) => s.mode);
  const setMode = useThemeStore((s) => s.setMode);
  const currentLang = i18n.language.startsWith('zh') ? 'zh' : 'en';

  return (
    <div className="space-y-8">
      <SectionHeader icon={Palette} title={t('appearance.title')} desc={t('appearance.desc')} />

      {/* Theme */}
      <SettingRow label={t('appearance.theme')} desc={t('appearance.themeDesc')}>
        <div className="flex gap-2">
          {THEME_OPTIONS.map(({ value, icon: Icon, labelKey }) => (
            <button
              key={value}
              onClick={() => setMode(value)}
              className={cn(
                'flex items-center gap-2 rounded-lg border px-3 py-2 text-sm transition-all',
                mode === value
                  ? 'border-primary/40 bg-primary/10 text-primary'
                  : 'border-border/60 bg-card/40 text-muted-foreground hover:border-border hover:text-foreground'
              )}
            >
              <Icon size={14} />
              {tTheme(labelKey as any)}
              {mode === value && <Check size={12} className="ml-0.5" />}
            </button>
          ))}
        </div>
      </SettingRow>

      {/* Language */}
      <SettingRow label={t('appearance.language')} desc={t('appearance.languageDesc')}>
        <div className="flex gap-2">
          {LANGS.map(({ code, label }) => (
            <button
              key={code}
              onClick={() => i18n.changeLanguage(code)}
              className={cn(
                'flex items-center gap-2 rounded-lg border px-3 py-2 text-sm transition-all',
                currentLang === code
                  ? 'border-primary/40 bg-primary/10 text-primary'
                  : 'border-border/60 bg-card/40 text-muted-foreground hover:border-border hover:text-foreground'
              )}
            >
              {label}
              {currentLang === code && <Check size={12} />}
            </button>
          ))}
        </div>
      </SettingRow>
    </div>
  );
}

// ── Section: Cluster ──────────────────────────────────────────────────────────

function ClusterSection() {
  const { t } = useTranslation('settings');
  const [testing, setTesting] = useState(false);
  const [testResult, setTestResult] = useState<{ ok: boolean; latency?: number; msg?: string } | null>(null);

  const { data: cfg, isLoading } = useRuntimeConfig();

  const handleTest = async () => {
    setTesting(true);
    setTestResult(null);
    const t0 = performance.now();
    try {
      await clusterApi.config();
      const latency = Math.round(performance.now() - t0);
      setTestResult({ ok: true, latency });
    } catch (e) {
      setTestResult({ ok: false, msg: e instanceof Error ? e.message : String(e) });
    } finally {
      setTesting(false);
    }
  };

  return (
    <div className="space-y-8">
      <SectionHeader icon={Plug} title={t('cluster.title')} desc={t('cluster.desc')} />

      <SettingRow label={t('cluster.endpoint')} desc={t('cluster.endpointDesc')}>
        <div className="space-y-3">
          <div className="flex items-center gap-2">
            <div className="flex-1 rounded-lg border border-border/60 bg-card/40 px-3 py-2 font-mono text-sm text-foreground/70">
              {cfg?.apiEndpoint ?? `${window.location.origin}/cubeapi/v1`}
            </div>
            <button
              onClick={handleTest}
              disabled={testing}
              className="inline-flex items-center gap-1.5 rounded-lg border border-border/60 bg-card/40 px-3 py-2 text-sm text-muted-foreground hover:border-primary/30 hover:text-foreground transition-colors disabled:opacity-50"
            >
              {testing ? <Loader2 size={13} className="animate-spin" /> : <Wifi size={13} />}
              {t('cluster.test')}
            </button>
          </div>

          {testResult && (
            <div className={cn(
              'flex items-center gap-2 rounded-lg border px-3 py-2 text-sm animate-fade-in',
              testResult.ok
                ? 'border-cube-ok/20 bg-cube-ok/[0.06] text-cube-ok'
                : 'border-cube-err/20 bg-cube-err/[0.06] text-cube-err'
            )}>
              {testResult.ok
                ? <><Wifi size={13} /> {t('cluster.connected')} · {testResult.latency}ms</>
                : <><WifiOff size={13} /> {testResult.msg}</>
              }
            </div>
          )}
        </div>
      </SettingRow>

      {/* Runtime info */}
      <SettingRow label={t('cluster.runtime')} desc={t('cluster.runtimeDesc')}>
        {isLoading ? (
          <div className="space-y-2">
            {[1,2,3,4].map(i => <div key={i} className="h-4 w-48 animate-pulse rounded bg-muted/60" />)}
          </div>
        ) : (
          <dl className="space-y-2 text-sm">
            {([
              { label: t('cluster.sandboxDomain'), value: cfg?.sandboxDomain ?? '—',  numeric: false },
              { label: t('cluster.instanceType'),  value: cfg?.instanceType ?? '—',   numeric: false },
              { label: t('cluster.rateLimit'),     value: `${cfg?.rateLimitPerSec ?? '—'} req/s`, numeric: true },
              { label: t('cluster.auth'),          value: cfg?.authEnabled ? t('cluster.authOn') : t('cluster.authOff'), numeric: false },
            ] as Array<{ label: string; value: string; numeric: boolean }>).map(({ label, value, numeric }) => (
              <div key={label} className="flex items-center gap-3">
                <span className="w-36 text-muted-foreground">{label}</span>
                <span className={cn('text-foreground/90', numeric && 'text-num')}>{value}</span>
              </div>
            ))}
          </dl>
        )}
      </SettingRow>
    </div>
  );
}

// ── Section: Account ──────────────────────────────────────────────────────────

function AccountSection() {
  const { t } = useTranslation('auth');
  const navigate = useNavigate();
  const username = getSessionUser() || 'admin';
  const [oldPassword, setOldPassword] = useState('');
  const [newPassword, setNewPassword] = useState('');
  const [confirm, setConfirm] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);

  const handleLogout = async () => {
    try {
      await authApi.logout();
    } catch {
      // ignore network errors on logout
    }
    clearSession();
    navigate('/login', { replace: true });
  };

  const handleChangePassword = async (e: React.FormEvent) => {
    e.preventDefault();
    setMsg(null);
    if (newPassword.length < 4) {
      setMsg({ ok: false, text: t('changePassword.tooShort') });
      return;
    }
    if (newPassword !== confirm) {
      setMsg({ ok: false, text: t('changePassword.mismatch') });
      return;
    }
    setSubmitting(true);
    try {
      await authApi.changePassword({ username, oldPassword, newPassword });
      setMsg({ ok: true, text: t('changePassword.success') });
      setOldPassword('');
      setNewPassword('');
      setConfirm('');
    } catch (err) {
      const text =
        err instanceof ApiError && err.status === 401
          ? err.message
          : err instanceof Error
            ? err.message
            : t('changePassword.error');
      setMsg({ ok: false, text });
    } finally {
      setSubmitting(false);
    }
  };

  const inputClass =
    'w-full rounded-lg border border-border/60 bg-card/40 px-3 py-2 text-sm outline-none transition-colors focus:border-primary/50 focus:ring-2 focus:ring-primary/15';

  return (
    <div className="space-y-8">
      <SectionHeader icon={UserCog} title={t('account.title')} desc={t('account.description')} />

      <SettingRow label={t('account.title')} desc={t('account.loggedInAs', { username })}>
        <button
          onClick={handleLogout}
          className="inline-flex items-center gap-1.5 rounded-lg border border-border/60 bg-card/40 px-3 py-2 text-sm text-muted-foreground transition-colors hover:border-rose-400/40 hover:text-rose-500"
        >
          <LogOut size={14} />
          {t('account.logout')}
        </button>
      </SettingRow>

      <SettingRow label={t('changePassword.title')}>
        <form onSubmit={handleChangePassword} className="max-w-sm space-y-3">
          <input
            type="password"
            className={inputClass}
            placeholder={t('changePassword.oldPlaceholder')}
            autoComplete="current-password"
            value={oldPassword}
            onChange={(e) => setOldPassword(e.target.value)}
          />
          <input
            type="password"
            className={inputClass}
            placeholder={t('changePassword.newPlaceholder')}
            autoComplete="new-password"
            value={newPassword}
            onChange={(e) => setNewPassword(e.target.value)}
          />
          <input
            type="password"
            className={inputClass}
            placeholder={t('changePassword.confirmPlaceholder')}
            autoComplete="new-password"
            value={confirm}
            onChange={(e) => setConfirm(e.target.value)}
          />
          <button
            type="submit"
            disabled={submitting || !oldPassword || !newPassword}
            className="inline-flex items-center gap-1.5 rounded-lg bg-primary px-3 py-2 text-sm font-medium text-primary-foreground transition-opacity hover:opacity-90 disabled:opacity-50"
          >
            {submitting ? <Loader2 size={14} className="animate-spin" /> : <KeyRound size={14} />}
            {submitting ? t('changePassword.submitting') : t('changePassword.submit')}
          </button>
          {msg && (
            <p className={cn('text-sm', msg.ok ? 'text-cube-emerald' : 'text-rose-500')}>{msg.text}</p>
          )}
        </form>
      </SettingRow>
    </div>
  );
}

// ── Section: Shortcuts ────────────────────────────────────────────────────────

const isMac = typeof navigator !== 'undefined' && /mac/i.test(navigator.platform);
const MOD = isMac ? '⌘' : 'Ctrl';

const SHORTCUTS: { action: string; keys: string[] }[] = [
  { action: 'shortcut.commandPalette', keys: [MOD, 'K'] },
  { action: 'shortcut.escape',         keys: ['Esc'] },
  { action: 'shortcut.refresh',        keys: ['R'] },
  { action: 'shortcut.helpShortcuts',  keys: ['?'] },
];

function Kbd({ children }: { children: string }) {
  return (
    <kbd className="inline-flex items-center justify-center rounded border border-border bg-muted/60 px-1.5 py-0.5 font-mono text-xs text-foreground/80 min-w-[22px]">
      {children}
    </kbd>
  );
}

function ShortcutsSection() {
  const { t } = useTranslation('settings');
  return (
    <div className="space-y-8">
      <SectionHeader icon={Keyboard} title={t('shortcuts.title')} desc={t('shortcuts.desc')} />
      <div className="rounded-xl border border-border/60 bg-card/40 divide-y divide-border/40">
        {SHORTCUTS.map(({ action, keys }) => (
          <div key={action} className="flex items-center justify-between px-5 py-3.5">
            <span className="text-sm text-foreground/80">{t(action as any)}</span>
            <div className="flex items-center gap-1">
              {keys.map((k, i) => (
                <span key={i} className="flex items-center gap-1">
                  {i > 0 && (
                    <span className="text-muted-foreground/40 text-xs px-0.5">
                      {keys.length === 2 && keys[0] === 'G' && i === 1 ? 'then' : '+'}
                    </span>
                  )}
                  <Kbd>{k}</Kbd>
                </span>
              ))}
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}

// ── Section: About ────────────────────────────────────────────────────────────

function AboutSection() {
  const { t } = useTranslation('settings');
  const { data: cfg } = useRuntimeConfig();
  const version = useControlPlaneVersion();

  return (
    <div className="space-y-8">
      <SectionHeader icon={Info} title={t('about.title')} desc={t('about.desc')} />

      <div className="rounded-xl border border-border/60 bg-card/40 divide-y divide-border/40">
        {([
          { label: t('about.version'),     value: `v${version}`,                                             mono: true  },
          { label: t('about.cubeApi'),     value: cfg?.apiEndpoint ?? `${window.location.origin}/cubeapi/v1`, mono: true  },
          { label: t('about.instanceType'),value: cfg?.instanceType ?? '—',                                   mono: false },
        ] as Array<{ label: string; value: string; mono: boolean }>).map(({ label, value, mono }) => (
          <div key={label} className="flex items-center justify-between px-5 py-3.5">
            <span className="text-sm text-muted-foreground">{label}</span>
            <span className={cn('text-sm text-foreground/90', mono && 'font-mono')}>{value}</span>
          </div>
        ))}
      </div>

      {/* links */}
      <div className="flex gap-3">
        {[
          { label: 'GitHub', href: 'https://github.com/tencentcloud/CubeSandbox' },
          { label: t('about.docs'), href: 'https://github.com/tencentcloud/CubeSandbox/wiki' },
        ].map(({ label, href }) => (
          <a
            key={label}
            href={href}
            target="_blank"
            rel="noopener noreferrer"
            className="inline-flex items-center gap-1.5 rounded-lg border border-border/60 bg-card/40 px-3 py-2 text-sm text-muted-foreground hover:border-primary/30 hover:text-foreground transition-colors"
          >
            {label}
            <ExternalLink size={12} />
          </a>
        ))}
      </div>
    </div>
  );
}

// ── Shared primitives ─────────────────────────────────────────────────────────

function SectionHeader({ icon: Icon, title, desc }: { icon: React.ElementType; title: string; desc: string }) {
  return (
    <div className="flex items-start gap-3 pb-2 border-b border-border/40">
      <span className="mt-0.5 flex h-8 w-8 shrink-0 items-center justify-center rounded-lg bg-muted/40 border border-border/60">
        <Icon size={15} className="text-muted-foreground/80" />
      </span>
      <div>
        <h2 className="text-base font-semibold">{title}</h2>
        <p className="text-sm text-muted-foreground mt-0.5">{desc}</p>
      </div>
    </div>
  );
}

function SettingRow({ label, desc, children }: { label: string; desc?: string; children: React.ReactNode }) {
  return (
    <div className="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
      <div className="sm:w-56 shrink-0">
        <p className="text-sm font-medium text-foreground/90">{label}</p>
        {desc && <p className="text-xs text-muted-foreground mt-0.5 leading-relaxed">{desc}</p>}
      </div>
      <div className="flex-1">{children}</div>
    </div>
  );
}

// ── Page ──────────────────────────────────────────────────────────────────────

const SECTION_COMPONENTS: Record<string, React.ComponentType> = {
  appearance: AppearanceSection,
  cluster:    ClusterSection,
  account:    AccountSection,
  shortcuts:  ShortcutsSection,
  about:      AboutSection,
};

export default function SettingsPage() {
  const { t } = useTranslation('settings');
  const location = useLocation();
  const defaultTab = new URLSearchParams(location.search).get('tab') ?? 'appearance';
  const [active, setActive] = useState<string>(
    SECTIONS.some(s => s.key === defaultTab) ? defaultTab : 'appearance'
  );
  const ActiveSection = SECTION_COMPONENTS[active] ?? AppearanceSection;

  return (
    <div className="animate-fade-in py-8">
      {/* page header */}
      <div className="flex items-center gap-3 border-b border-border/50 pb-6 mb-8">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">{t('title')}</h1>
          <p className="text-sm text-muted-foreground mt-0.5">{t('subtitle')}</p>
        </div>
      </div>

      {/* two-column layout */}
      <div className="flex gap-10">
        <SettingsSidebar active={active} onChange={setActive} />
        <div className="flex-1 min-w-0">
          <ActiveSection />
        </div>
      </div>
    </div>
  );
}
