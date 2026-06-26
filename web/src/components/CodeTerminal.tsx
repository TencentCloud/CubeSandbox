// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useState, useCallback, useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { useMutation } from '@tanstack/react-query';
import { sandboxApi, type ExecCodeResult, type JupyterResult } from '@/api/client';
import { CodeEditor, type EditorLanguage } from '@/components/CodeEditor';
import { Card, CardTitle, CardDescription } from '@/components/ui/card';
import { Button } from '@/components/ui/button';
import { Badge } from '@/components/ui/badge';
import { Play, Loader2, Terminal } from 'lucide-react';
import DOMPurify from 'dompurify';

type SupportedLang = 'python' | 'bash';

const LANG_OPTIONS: { value: SupportedLang; label: string; editorLang: EditorLanguage }[] = [
  { value: 'python', label: 'Python', editorLang: 'python' },
  { value: 'bash', label: 'Bash', editorLang: 'shell' },
];

const DEFAULT_CODE: Record<SupportedLang, string> = {
  python: 'print("Hello, Sandbox!")',
  bash: 'echo "Hello, Sandbox!"',
};

interface CodeTerminalProps {
  sandboxId: string;
  disabled?: boolean;
}

/** Render Jupyter rich results (HTML tables, images, etc.). */
function RichResults({ results }: { results: JupyterResult[] }) {
  return (
    <div className="space-y-2">
      {results.map((r, i) => (
        <RichResultItem key={i} result={r} />
      ))}
    </div>
  );
}

function RichResultItem({ result }: { result: JupyterResult }) {
  // Prefer HTML → PNG → SVG → Markdown → text
  if (result.html) {
    return (
      <div
        className="jupyter-html-output max-h-[300px] overflow-auto rounded-md bg-white p-2 text-xs ring-1 ring-border/60 [&_table]:border-collapse [&_td]:border [&_td]:px-2 [&_td]:py-1 [&_th]:border [&_th]:px-2 [&_th]:py-1 [&_th]:bg-muted/40"
        dangerouslySetInnerHTML={{
          __html: DOMPurify.sanitize(result.html, {
            FORBID_TAGS: ['svg', 'use', 'animate', 'animateTransform', 'set'],
            SANITIZE_NAMED_PROPS: true,
          }),
        }}
      />
    );
  }
  if (result.png) {
    // Jupyter returns base64-encoded PNG
    const src = result.png.startsWith('data:')
      ? result.png
      : `data:image/png;base64,${result.png}`;
    return (
      <div className="max-h-[300px] overflow-auto rounded-md bg-white p-2 ring-1 ring-border/60">
        <img src={src} alt="output" className="max-w-full" />
      </div>
    );
  }
  if (result.jpeg) {
    const src = result.jpeg.startsWith('data:')
      ? result.jpeg
      : `data:image/jpeg;base64,${result.jpeg}`;
    return (
      <div className="max-h-[300px] overflow-auto rounded-md bg-white p-2 ring-1 ring-border/60">
        <img src={src} alt="output" className="max-w-full" />
      </div>
    );
  }

  if (result.markdown) {
    return (
      <pre className="max-h-[240px] overflow-auto rounded-md bg-muted/60 p-3 text-xs leading-relaxed ring-1 ring-border/60 whitespace-pre-wrap">
        {result.markdown}
      </pre>
    );
  }
  if (result.text) {
    return (
      <pre className="max-h-[240px] overflow-auto rounded-md bg-muted/60 p-3 font-mono text-xs leading-relaxed ring-1 ring-border/60">
        {result.text}
      </pre>
    );
  }
  return null;
}

export function CodeTerminal({ sandboxId, disabled = false }: CodeTerminalProps) {
  const { t } = useTranslation('sandboxDetail');
  const [language, setLanguage] = useState<SupportedLang>('python');
  const [code, setCode] = useState(DEFAULT_CODE.python);
  const [result, setResult] = useState<ExecCodeResult | null>(null);

  const exec = useMutation({
    mutationFn: () =>
      sandboxApi.execCode(sandboxId, {
        code,
        language,
        timeout_secs: 30,
      }),
    onSuccess: setResult,
  });

  const handleLangChange = useCallback((val: SupportedLang) => {
    setLanguage(val);
    setCode(DEFAULT_CODE[val]);
    setResult(null);
  }, []);

  const editorLang = LANG_OPTIONS.find((l) => l.value === language)!.editorLang;

  // Separate rich Jupyter results from plain text results
  const richResults = useMemo(() => {
    if (!result?.results?.length) return null;
    // Only show results that have non-text rich content (html, png, svg, etc.)
    // or main results with text
    return result.results.filter(
      (r) => r.html || r.png || r.jpeg || r.svg || r.markdown || (r.is_main_result && r.text),
    );
  }, [result?.results]);

  return (
    <Card className="overflow-hidden">
      <div className="border-b border-border/60 bg-muted/30 px-5 py-3.5">
        <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
          <div className="min-w-0">
            <CardTitle className="flex items-center gap-2 text-sm">
              <Terminal size={15} />
              {t('terminal')}
            </CardTitle>
            <CardDescription className="mt-0.5">{t('terminalDesc')}</CardDescription>
          </div>
          <div className="flex flex-shrink-0 items-center gap-2.5">
            <select
              value={language}
              onChange={(e) => handleLangChange(e.target.value as SupportedLang)}
              className="h-8 rounded-md border border-border/60 bg-background px-3 text-xs outline-none focus:ring-1 focus:ring-ring"
            >
              {LANG_OPTIONS.map((opt) => (
                <option key={opt.value} value={opt.value}>
                  {opt.label}
                </option>
              ))}
            </select>
            <Button
              size="sm"
              onClick={() => exec.mutate()}
              disabled={disabled || exec.isPending || !code.trim()}
              className="gap-1.5 px-4"
            >
              {exec.isPending ? (
                <Loader2 size={13} className="animate-spin" />
              ) : (
                <Play size={13} />
              )}
              {t('terminalRun')}
            </Button>
          </div>
        </div>
      </div>

      {/* Code input */}
      <div className="p-4">
        <CodeEditor
          value={code}
          language={editorLang}
          onChange={setCode}
          height={560}
          minHeight={400}
          readOnly={disabled}
          ariaLabel={t('terminalCodeLabel')}
        />
      </div>

      {/* Output */}
      {exec.isPending && (
        <div className="flex items-center gap-2 border-t border-border/40 px-5 py-2.5 text-xs text-muted-foreground">
          <Loader2 size={12} className="animate-spin" />
          {t('terminalRunning')}
        </div>
      )}

      {result && (
        <div className="space-y-3 border-t border-border/40 px-5 py-4">
          <div className="flex items-center gap-2.5">
            <span className="text-xs font-medium">{t('terminalResult')}</span>
            <Badge tone={result.success ? 'ok' : 'err'}>
              {result.success ? t('terminalSuccess') : t('terminalFailed')}
            </Badge>
            {!result.success && (
              <span className="text-muted-foreground">
                {t('terminalExitCode')}: {result.exit_code}
              </span>
            )}
            <span className="text-muted-foreground">
              {result.elapsed_ms}ms
            </span>
          </div>

          {result.stdout && (
            <pre className="max-h-[240px] overflow-auto rounded-md bg-muted/60 p-3 font-mono text-xs leading-relaxed ring-1 ring-border/60">
              {result.stdout}
            </pre>
          )}

          {result.stderr && (
            <pre className="max-h-[160px] overflow-auto rounded-md bg-destructive/10 p-3 font-mono text-xs leading-relaxed ring-1 ring-destructive/20">
              {result.stderr}
            </pre>
          )}

          {richResults && richResults.length > 0 && (
            <RichResults results={richResults} />
          )}

          {!result.stdout && !result.stderr && !richResults?.length && (
            <p className="text-xs text-muted-foreground">{t('terminalNoOutput')}</p>
          )}
        </div>
      )}
    </Card>
  );
}
