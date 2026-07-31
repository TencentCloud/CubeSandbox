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
 * Redirecting these would break the developer's local workflow rather than
 * protect it: the sandbox has its own filesystem, so a `git` command executed
 * there would operate on a different repository than the one OpenCode is
 * editing through `read` / `write` / `edit`.
 *
 * Set CUBE_OPENCODE_PASSTHROUGH to a comma-separated list to change this.
 */
const DEFAULT_PASSTHROUGH = ["git", "gh", "opencode"];

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
 * back to a constant. A wrong-but-stable key only costs sandbox reuse
 * granularity; it never causes a host escape.
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
      if (!output || !output.args) return;

      const original = output.args.command;
      if (typeof original !== "string" || original.trim() === "") return;

      // Idempotence guard. Multiple plugins may observe the same call, and a
      // second rewrite would nest the backend invocation inside itself.
      if (original.includes("exec_backend.py")) return;

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
