// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { isMockEnabled } from './mockFlag';

export type TerminalMessage =
  | { type: 'input'; data: string }
  | { type: 'output'; data: string }
  | { type: 'resize'; cols: number; rows: number }
  | { type: 'error'; message: string }
  | { type: 'close'; reason?: string };

export interface TerminalSocket {
  readonly readyState: number;
  onopen: (() => void) | null;
  onmessage: ((message: TerminalMessage) => void) | null;
  onclose: ((event?: { code?: number; reason?: string }) => void) | null;
  onerror: ((event: { message: string }) => void) | null;
  sendInput(data: string): void;
  sendResize(cols: number, rows: number): void;
  close(): void;
}

const READY_STATE_CONNECTING = 0;
const READY_STATE_OPEN = 1;
const READY_STATE_CLOSING = 2;
const READY_STATE_CLOSED = 3;

function utf8ToBase64(s: string): string {
  const bytes = new TextEncoder().encode(s);
  let binary = '';
  for (let i = 0; i < bytes.byteLength; i++) {
    binary += String.fromCharCode(bytes[i]);
  }
  return btoa(binary);
}

export { utf8ToBase64 };

export function base64ToUtf8(b64: string): string {
  const binary = atob(b64);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) {
    bytes[i] = binary.charCodeAt(i);
  }
  return new TextDecoder().decode(bytes);
}

export function closeReasonMessage(code?: number, reason?: string): string {
  if (reason) return reason;
  switch (code) {
    case 1000:
      return 'normal closure';
    case 1001:
      return 'endpoint going away';
    case 1006:
      return 'connection lost';
    case 1008:
      return 'policy violation';
    case 1011:
      return 'server error';
    default:
      return code ? `closed (${code})` : 'connection closed';
  }
}

/**
 * Create a WebSocket connection to the sandbox terminal endpoint.
 * In mock mode this returns a local fake shell so the UI can be exercised
 * without the backend bridge implemented in step 3.
 */
export function createTerminalSocket(sandboxID: string, containerID?: string): TerminalSocket {
  if (isMockEnabled()) {
    return new MockTerminalSocket(sandboxID);
  }
  return new NativeTerminalSocket(sandboxID, containerID);
}

class NativeTerminalSocket implements TerminalSocket {
  private ws: WebSocket;
  private inputQueue: string[] = [];
  private pendingResize: { cols: number; rows: number } | null = null;

  onopen: (() => void) | null = null;
  onmessage: ((message: TerminalMessage) => void) | null = null;
  onclose: ((event?: { code?: number; reason?: string }) => void) | null = null;
  onerror: ((event: { message: string }) => void) | null = null;

  constructor(sandboxID: string, containerID?: string) {
    const apiKey = localStorage.getItem('cube.apiKey') ?? '';
    const accessToken = localStorage.getItem('cube.session') ?? '';
    const params = new URLSearchParams();
    if (apiKey) params.set('api_key', apiKey);
    if (accessToken) params.set('access_token', accessToken);
    if (containerID) params.set('container', containerID);

    const query = params.toString();
    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    const url = `${protocol}//${window.location.host}/cubeapi/v1/sandboxes/${encodeURIComponent(sandboxID)}/terminal/ws${query ? `?${query}` : ''}`;

    this.ws = new WebSocket(url);
    this.ws.onopen = () => {
      this.flushQueue();
      this.onopen?.();
    };
    this.ws.onmessage = (event) => {
      try {
        const message = JSON.parse(event.data as string) as TerminalMessage;
        this.onmessage?.(message);
      } catch {
        this.onmessage?.({ type: 'output', data: utf8ToBase64(String(event.data)) });
      }
    };
    this.ws.onclose = (event) => this.onclose?.({ code: event.code, reason: event.reason });
    this.ws.onerror = () => this.onerror?.({ message: 'WebSocket error' });
  }

  get readyState(): number {
    return this.ws.readyState;
  }

  sendInput(data: string): void {
    if (this.ws.readyState === WebSocket.OPEN) {
      this.ws.send(JSON.stringify({ type: 'input', data: utf8ToBase64(data) }));
    } else if (this.ws.readyState === WebSocket.CONNECTING) {
      this.inputQueue.push(data);
    }
  }

  sendResize(cols: number, rows: number): void {
    if (this.ws.readyState === WebSocket.OPEN) {
      this.ws.send(JSON.stringify({ type: 'resize', cols, rows }));
    } else if (this.ws.readyState === WebSocket.CONNECTING) {
      this.pendingResize = { cols, rows };
    }
  }

  close(): void {
    this.inputQueue = [];
    this.pendingResize = null;
    this.ws.close();
  }

  private flushQueue(): void {
    while (this.inputQueue.length > 0) {
      const data = this.inputQueue.shift()!;
      this.sendInput(data);
    }
    if (this.pendingResize) {
      this.sendResize(this.pendingResize.cols, this.pendingResize.rows);
      this.pendingResize = null;
    }
  }
}

/**
 * In-memory mock terminal for local development. It echoes typed characters and
 * evaluates a tiny set of fake shell commands so the xterm cursor, resize and
 * scrollback behavior can be verified before the real backend exists.
 */
class MockTerminalSocket implements TerminalSocket {
  readyState = READY_STATE_CONNECTING;

  onopen: (() => void) | null = null;
  onmessage: ((message: TerminalMessage) => void) | null = null;
  onclose: ((event?: { code?: number; reason?: string }) => void) | null = null;
  onerror: ((event: { message: string }) => void) | null = null;

  private sandboxID: string;
  private line = '';
  private timer: number | null = null;
  private closed = false;

  constructor(sandboxID: string) {
    this.sandboxID = sandboxID;
    this.timer = window.setTimeout(() => {
      if (this.closed) return;
      this.readyState = READY_STATE_OPEN;
      this.onopen?.();
      this.emitOutput(
        '\x1b[1;36mCubeSandbox mock terminal\x1b[0m\r\n' +
          '\x1b[90msandbox:\x1b[0m \x1b[33m' +
          sandboxID +
          '\x1b[0m\r\n' +
          '\x1b[90mTry: help, ls, colors, dmesg, clear, exit\x1b[0m\r\n\r\n' +
          this.prompt(),
      );
    }, 400);
  }

  private prompt(): string {
    return '\x1b[32m$\x1b[0m ';
  }

  sendInput(data: string): void {
    if (this.readyState !== READY_STATE_OPEN || this.closed) return;

    for (const ch of data) {
      const code = ch.charCodeAt(0);
      if (ch === '\r') {
        this.emitOutput('\r\n');
        this.runCommand(this.line.trim());
        this.line = '';
        this.emitOutput(this.prompt());
      } else if (ch === '\x7f' || ch === '\b') {
        if (this.line.length > 0) {
          this.line = this.line.slice(0, -1);
          this.emitOutput('\b \b');
        }
      } else if (code === 3) {
        // Ctrl+C
        this.line = '';
        this.emitOutput('^C\r\n' + this.prompt());
      } else if (code === 4) {
        // Ctrl+D
        this.emitOutput('\x1b[33mexit\x1b[0m\r\n');
        this.close();
        return;
      } else if (code === 9) {
        // Tab — no completion in mock mode
      } else if (code >= 32 && code < 127) {
        this.line += ch;
        this.emitOutput(ch);
      }
    }
  }

  sendResize(_cols: number, _rows: number): void {
    // Mock shell ignores resizes.
  }

  close(): void {
    if (this.closed) return;
    this.closed = true;
    this.readyState = READY_STATE_CLOSED;
    if (this.timer) {
      window.clearTimeout(this.timer);
      this.timer = null;
    }
    this.onmessage?.({ type: 'close' });
    this.onclose?.({ code: 1000, reason: 'closed by client' });
  }

  private emitOutput(text: string): void {
    this.onmessage?.({ type: 'output', data: utf8ToBase64(text) });
  }

  private runCommand(cmd: string): void {
    if (!cmd) return;
    const [name, ...args] = cmd.split(/\s+/);
    switch (name) {
      case 'help':
        this.emitOutput(
          '\x1b[1mAvailable commands:\x1b[0m\r\n' +
            '  \x1b[32mhelp\x1b[0m      show this help\r\n' +
            '  \x1b[32mls\x1b[0m       list files\r\n' +
            '  \x1b[32mpwd\x1b[0m      print working directory\r\n' +
            '  \x1b[32muname\x1b[0m    print system info\r\n' +
            '  \x1b[32mecho\x1b[0m     echo arguments\r\n' +
            '  \x1b[32mdate\x1b[0m     print current time\r\n' +
            '  \x1b[32mwhoami\x1b[0m    print current user\r\n' +
            '  \x1b[32mcolors\x1b[0m    ANSI color test\r\n' +
            '  \x1b[32mdmesg\x1b[0m    long colored output (scrollback demo)\r\n' +
            '  \x1b[32mclear\x1b[0m    clear the screen\r\n' +
            '  \x1b[32mexit\x1b[0m     close the terminal\r\n',
        );
        break;
      case 'ls':
        this.emitOutput(
          '\x1b[34mdocs\x1b[0m/      \x1b[34mworkspace\x1b[0m/  \x1b[32mrun.sh\x1b[0m*   \x1b[0mREADME.md\x1b[0m   \x1b[35mconfig.yaml\x1b[0m\r\n',
        );
        break;
      case 'pwd':
        this.emitOutput('/home/sandbox\r\n');
        break;
      case 'uname':
        this.emitOutput('CubeSandbox mock kernel 0.5.0 x86_64\r\n');
        break;
      case 'echo':
        this.emitOutput(`${args.join(' ')}\r\n`);
        break;
      case 'date':
        this.emitOutput(`\x1b[33m${new Date().toISOString()}\x1b[0m\r\n`);
        break;
      case 'whoami':
        this.emitOutput('\x1b[32mmock\x1b[0m\r\n');
        break;
      case 'colors':
        this.emitOutput(
          '\x1b[31mred\x1b[0m  \x1b[32mgreen\x1b[0m  \x1b[33myellow\x1b[0m  ' +
            '\x1b[34mblue\x1b[0m  \x1b[35mmagenta\x1b[0m  \x1b[36mcyan\x1b[0m  \x1b[90mgray\x1b[0m\r\n' +
            '\x1b[1;31mred\x1b[0m  \x1b[1;32mgreen\x1b[0m  \x1b[1;33myellow\x1b[0m  ' +
            '\x1b[1;34mblue\x1b[0m  \x1b[1;35mmagenta\x1b[0m  \x1b[1;36mcyan\x1b[0m\r\n',
        );
        break;
      case 'dmesg':
        for (let i = 1; i <= 40; i++) {
          const color = i % 2 === 0 ? '\x1b[32m' : '\x1b[36m';
          this.emitOutput(
            `\x1b[90m[${String(i).padStart(3, '0')}.000000]\x1b[0m ${color}mock kernel: event ${i} logged\x1b[0m\r\n`,
          );
        }
        break;
      case 'clear':
        this.onmessage?.({ type: 'output', data: utf8ToBase64('\x1b[2J\x1b[H') });
        break;
      case 'exit':
        this.emitOutput('\x1b[33mlogout\x1b[0m\r\n');
        this.close();
        break;
      default:
        this.emitOutput(`\x1b[31m${name}: command not found\x1b[0m\r\n`);
    }
  }
}
