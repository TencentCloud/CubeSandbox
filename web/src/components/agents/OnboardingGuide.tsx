// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import { Check, KeyRound, Sparkles, Package } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { cn } from '@/lib/utils';

interface OnboardingGuideProps {
  loading: boolean;
  apiKeyConfigured: boolean;
  templateReady: boolean;
  onConfigureApiKey: () => void;
}

/**
 * First-run checklist shown on the AgentHub page until the prerequisites for
 * creating a digital assistant are met: a DeepSeek API key is configured and a
 * 龙虾助手 template is available from the template market.
 */
export function OnboardingGuide({
  loading,
  apiKeyConfigured,
  templateReady,
  onConfigureApiKey,
}: OnboardingGuideProps) {
  const { t } = useTranslation('agentHub');

  // Hide until readiness is known, and once everything is ready.
  if (loading) return null;
  if (apiKeyConfigured && templateReady) return null;

  return (
    <div className="rounded-2xl border border-primary/30 bg-primary/5 p-5 shadow-sm">
      <div className="flex items-center gap-2">
        <span className="flex h-8 w-8 items-center justify-center rounded-lg bg-primary/15 text-primary">
          <Sparkles size={16} />
        </span>
        <div>
          <h2 className="text-sm font-semibold">{t('onboarding.title')}</h2>
          <p className="text-xs text-muted-foreground">{t('onboarding.description')}</p>
        </div>
      </div>

      <ol className="mt-4 space-y-3">
        <Step
          index={1}
          done={apiKeyConfigured}
          icon={<KeyRound size={15} />}
          title={t('onboarding.steps.apiKey.title')}
          desc={t('onboarding.steps.apiKey.desc')}
          action={
            <Button size="sm" variant={apiKeyConfigured ? 'outline' : 'default'} onClick={onConfigureApiKey}>
              {apiKeyConfigured
                ? t('onboarding.steps.apiKey.doneAction')
                : t('onboarding.steps.apiKey.action')}
            </Button>
          }
        />
        <Step
          index={2}
          done={templateReady}
          icon={<Package size={15} />}
          title={t('onboarding.steps.template.title')}
          desc={t('onboarding.steps.template.desc')}
          action={
            <Link to="/store">
              <Button size="sm" variant={templateReady ? 'outline' : 'default'}>
                {templateReady
                  ? t('onboarding.steps.template.doneAction')
                  : t('onboarding.steps.template.action')}
              </Button>
            </Link>
          }
        />
      </ol>
    </div>
  );
}

function Step({
  index,
  done,
  icon,
  title,
  desc,
  action,
}: {
  index: number;
  done: boolean;
  icon: React.ReactNode;
  title: string;
  desc: string;
  action: React.ReactNode;
}) {
  return (
    <li className="flex items-center gap-3 rounded-xl border border-border/60 bg-background px-4 py-3">
      <span
        className={cn(
          'flex h-8 w-8 shrink-0 items-center justify-center rounded-full text-sm font-semibold',
          done
            ? 'bg-emerald-500/15 text-emerald-600 dark:text-emerald-300'
            : 'bg-muted text-muted-foreground'
        )}
      >
        {done ? <Check size={16} /> : index}
      </span>
      <div className="min-w-0 flex-1">
        <div className="flex items-center gap-1.5 text-sm font-medium">
          <span className="text-muted-foreground">{icon}</span>
          {title}
        </div>
        <p className="mt-0.5 text-xs text-muted-foreground">{desc}</p>
      </div>
      <div className="shrink-0">{action}</div>
    </li>
  );
}
