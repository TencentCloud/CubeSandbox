// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useMemo } from 'react';
import { cn } from '@/lib/utils';

// ── Python token highlighter ──────────────────────────────────────────────────
// Lightweight, dependency-free tokenizer for Python source. Splits a source
// string into line-by-line segments tagged with one of:
//   comment | keyword | string | number | function | decorator | builtin
// Plain text is the default. Intentionally simple — covers the tokens a reader
// needs to scan example code at a glance, with no grammar edge cases.

type TokenKind =
  | 'plain'
  | 'comment'
  | 'keyword'
  | 'string'
  | 'number'
  | 'function'
  | 'decorator'
  | 'builtin'
  | 'operator';

interface Token {
  kind: TokenKind;
  text: string;
}

const KEYWORDS = new Set([
  'and', 'as', 'assert', 'async', 'await', 'break', 'class', 'continue',
  'def', 'del', 'elif', 'else', 'except', 'finally', 'for', 'from',
  'global', 'if', 'import', 'in', 'is', 'lambda', 'nonlocal', 'not', 'or',
  'pass', 'raise', 'return', 'try', 'while', 'with', 'yield', 'match', 'case',
]);

const BUILTINS = new Set([
  'print', 'len', 'range', 'list', 'dict', 'set', 'tuple', 'str', 'int',
  'float', 'bool', 'bytes', 'open', 'type', 'isinstance', 'enumerate',
  'map', 'filter', 'zip', 'sum', 'min', 'max', 'abs', 'sorted', 'reversed',
  'any', 'all', 'True', 'False', 'None', 'self', 'cls',
]);

/**
 * Tokenize a single line of Python. Strings (single/double/triple-quoted
 * continued from a previous line are *not* handled across line boundaries —
 * examples are short and self-contained, this is good enough.
 */
function tokenizeLine(line: string): Token[] {
  const out: Token[] = [];
  let i = 0;
  let buf = '';

  const flush = (kind: TokenKind) => {
    if (buf) {
      out.push({ kind, text: buf });
      buf = '';
    }
  };
  const append = (kind: TokenKind, text: string) => {
    if (buf) {
      out.push({ kind: 'plain', text: buf });
      buf = '';
    }
    out.push({ kind, text });
  };

  while (i < line.length) {
    const ch = line[i];
    const rest = line.slice(i);

    // Comment to EOL
    if (ch === '#') {
      flush('plain');
      append('comment', line.slice(i));
      i = line.length;
      break;
    }

    // Triple-quoted string (single line — covers docstring headers)
    const triple = rest.match(/^(?:"""|''')/);
    if (triple) {
      flush('plain');
      const end = line.indexOf(triple[0], i + 3);
      if (end >= 0) {
        append('string', line.slice(i, end + 3));
        i = end + 3;
      } else {
        append('string', line.slice(i));
        i = line.length;
      }
      continue;
    }

    // Single/double quoted string
    if (ch === '"' || ch === '\'') {
      flush('plain');
      const quote = ch;
      let j = i + 1;
      while (j < line.length && line[j] !== quote) {
        if (line[j] === '\\' && j + 1 < line.length) j += 2;
        else j++;
      }
      const end = j < line.length ? j + 1 : line.length;
      append('string', line.slice(i, end));
      i = end;
      continue;
    }

    // Decorator
    if (ch === '@' && (i === 0 || /\s/.test(buf.slice(-1)))) {
      flush('plain');
      const m = rest.match(/^@[A-Za-z_][A-Za-z0-9_.]*/);
      if (m) {
        append('decorator', m[0]);
        i += m[0].length;
        continue;
      }
    }

    // Number
    if (/[0-9]/.test(ch)) {
      const m = rest.match(/^(?:0[xX][0-9A-Fa-f]+|0[oO][0-7]+|0[bB][01]+|\d+(?:\.\d+)?(?:[eE][+-]?\d+)?[jJ]?)/);
      if (m) {
        flush('plain');
        append('number', m[0]);
        i += m[0].length;
        continue;
      }
    }

    // Identifier
    if (/[A-Za-z_]/.test(ch)) {
      const m = rest.match(/^[A-Za-z_][A-Za-z0-9_]*/);
      if (m) {
        const word = m[0];
        buf += word;
        i += word.length;
        // Look ahead for "(" to mark as a call
        if (line[i] === '(') {
          if (KEYWORDS.has(word)) {
            flush('keyword');
          } else if (BUILTINS.has(word)) {
            flush('builtin');
          } else {
            // identifiers followed by "(" are calls → function highlight
            flush('plain');
            append('function', word);
          }
        } else {
          if (KEYWORDS.has(word)) {
            flush('keyword');
          } else if (BUILTINS.has(word) && /^\s|$|[)\]:,\.]/.test(line[i] ?? ' ')) {
            // Treat as builtin only when not a method call receiver
            flush('builtin');
          }
        }
        continue;
      }
    }

    // Operators / punctuation
    if (/[+\-*/%=<>!&|^~]/.test(ch)) {
      flush('plain');
      append('operator', ch);
      i++;
      continue;
    }

    buf += ch;
    i++;
  }
  flush('plain');
  return out;
}

// ── Public component ──────────────────────────────────────────────────────────

export interface CodeBlockProps {
  code: string;
  language?: string;
  /** Show line numbers in a leading gutter (default: true) */
  showLineNumbers?: boolean;
  className?: string;
}

/**
 * A minimal Python syntax-highlighted code block. Renders each line as a
 * flex row with a line-number gutter and token spans styled by `kind`. No
 * external deps, no virtualized scrolling — sized to the content.
 */
export function CodeBlock({
  code,
  language = 'python',
  showLineNumbers = true,
  className,
}: CodeBlockProps) {
  const lines = useMemo(() => code.replace(/\n$/, '').split('\n'), [code]);
  const tokenized = useMemo(() => lines.map(tokenizeLine), [lines]);

  return (
    <pre
      className={cn(
        'font-mono text-[12.5px] leading-[1.65] overflow-x-auto',
        'text-foreground/90',
        className,
      )}
    >
      <code className="block min-w-fit">
        {tokenized.map((tokens, lineIdx) => (
          <div
            key={lineIdx}
            className="flex items-start hover:bg-tint-bg-5 px-0"
          >
            {showLineNumbers && (
              <span
                aria-hidden
                className="select-none shrink-0 w-10 pr-3 text-right text-muted-foreground/30 text-num"
              >
                {lineIdx + 1}
              </span>
            )}
            <span className="flex-1 whitespace-pre">
              {tokens.length === 0 || (tokens.length === 1 && tokens[0].text === '')
                ? <span>&nbsp;</span>
                : tokens.map((t, i) => (
                    <span key={i} className={tokenClass(t.kind)}>
                      {t.text}
                    </span>
                  ))}
            </span>
          </div>
        ))}
      </code>
    </pre>
  );
}

function tokenClass(kind: TokenKind): string {
  switch (kind) {
    case 'comment':    return 'text-muted-foreground/50 italic';
    case 'keyword':    return 'text-cube-violet font-semibold';
    case 'string':     return 'text-cube-emerald';
    case 'number':     return 'text-cube-amber';
    case 'function':   return 'text-cube-cyan';
    case 'decorator':  return 'text-cube-amber/90';
    case 'builtin':    return 'text-primary';
    case 'operator':   return 'text-muted-foreground';
    default:           return '';
  }
}
