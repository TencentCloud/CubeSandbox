// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.
//
// Thin wrapper around @monaco-editor/react. We don't need the full Monaco
// bundle for read-only display; this component picks the smallest loader
// config that still gives us:
//   - JetBrains Mono via the project's fontsource registration
//   - Language-aware tokenization
//   - Line numbers + minimap off (read-only preview mode is supported via prop)

import Editor, { type OnMount, type OnChange } from '@monaco-editor/react';
import { useCallback, useRef } from 'react';
import { Loader2 } from 'lucide-react';
import { cn } from '@/lib/utils';

export type EditorLanguage = 'python' | 'go' | 'javascript' | 'typescript' | 'shell' | 'markdown';

export interface CodeEditorProps {
  value: string;
  language: EditorLanguage;
  onChange?: (value: string) => void;
  readOnly?: boolean;
  height?: number | string;
  className?: string;
  /** Minimum height for the editor viewport. Default: 360. */
  minHeight?: number;
  /** Show a minimap inside the editor. Default: false. */
  minimap?: boolean;
  /** Optional aria-label for the editor region. */
  ariaLabel?: string;
}

export function CodeEditor({
  value,
  language,
  onChange,
  readOnly = false,
  height = 420,
  className,
  minHeight = 360,
  minimap = false,
  ariaLabel,
}: CodeEditorProps) {
  const editorRef = useRef<unknown>(null);

  const handleMount: OnMount = useCallback((editor, monaco) => {
    editorRef.current = editor;
    // Define a CubeSandbox-flavoured dark theme once on mount; Monaco reuses
    // the same theme across all editor instances so this is cheap.
    monaco.editor.defineTheme('cubesandbox-dark', {
      base: 'vs-dark',
      inherit: true,
      rules: [
        { token: 'comment', foreground: '6b7280', fontStyle: 'italic' },
        { token: 'keyword', foreground: 'a78bfa' },
        { token: 'string', foreground: '34d399' },
        { token: 'number', foreground: 'fbbf24' },
        { token: 'type', foreground: '22d3ee' },
      ],
      colors: {
        'editor.background': '#0b0d12',
        'editor.foreground': '#e5e7eb',
        'editorLineNumber.foreground': '#3f3f46',
        'editorLineNumber.activeForeground': '#a1a1aa',
        'editor.lineHighlightBackground': '#11141a',
        'editorCursor.foreground': '#22d3ee',
        'editor.selectionBackground': '#22d3ee33',
        'editor.inactiveSelectionBackground': '#22d3ee22',
        'editorIndentGuide.background1': '#1f2937',
        'editorIndentGuide.activeBackground1': '#374151',
        'editorGutter.background': '#0b0d12',
      },
    });
    monaco.editor.setTheme('cubesandbox-dark');
  }, []);

  const handleChange: OnChange = useCallback(
    (next) => {
      if (readOnly) return;
      onChange?.(next ?? '');
    },
    [onChange, readOnly],
  );

  return (
    <div
      className={cn(
        'relative overflow-hidden rounded-lg border border-border/60 bg-[#0b0d12]',
        className,
      )}
      style={{ minHeight }}
      aria-label={ariaLabel ?? 'Code editor'}
    >
      <Editor
        height={height}
        language={language === 'shell' ? 'shell' : language}
        value={value}
        onChange={handleChange}
        onMount={handleMount}
        theme="vs-dark"
        loading={
          <div className="flex h-full min-h-[inherit] w-full items-center justify-center gap-2 text-xs text-muted-foreground">
            <Loader2 className="h-4 w-4 animate-spin" />
            Loading editor…
          </div>
        }
        options={{
          readOnly,
          minimap: { enabled: minimap },
          fontFamily:
            '"JetBrains Mono Variable", "JetBrains Mono", ui-monospace, SFMono-Regular, Menlo, monospace',
          fontLigatures: true,
          fontSize: 12.5,
          lineHeight: 1.65,
          scrollBeyondLastLine: false,
          smoothScrolling: true,
          cursorBlinking: 'smooth',
          cursorSmoothCaretAnimation: 'on',
          renderLineHighlight: 'all',
          renderWhitespace: 'selection',
          tabSize: 4,
          automaticLayout: true,
          wordWrap: 'on',
          fixedOverflowWidgets: true,
          padding: { top: 12, bottom: 12 },
          guides: { indentation: true, highlightActiveIndentation: true },
          scrollbar: {
            vertical: 'auto',
            horizontal: 'auto',
            verticalScrollbarSize: 8,
            horizontalScrollbarSize: 8,
          },
        }}
      />
    </div>
  );
}