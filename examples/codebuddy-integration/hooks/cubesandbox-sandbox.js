// cubesandbox-sandbox.js — CodeBuddy plugin that routes bash tool calls
// through a host-side CubeSandbox MicroVM instead of executing them on the
// host directly.
//
// Drop this file (and any sibling files in the same directory) into one of
// the CodeBuddy plugin directories to install:
//
//   ~/.config/codebuddy/plugins/cubesandbox-sandbox.js   (global)
//   .codebuddy/plugins/cubesandbox-sandbox.js            (project)
//
// Then add `CUBE_*` settings to the CodeBuddy config. Only whitelisted CUBE_*
// keys are read; LLM provider credentials are never copied here.
//
// Behaviour:
//   - "tool.execute.before" for the `bash` tool:
//       * spawns `python3 <sandbox_exec.py> --cmd <quoted>` from this plugin
//         file's directory
//       * reads its stdout (which contains the command's stdout) and stderr
//       * replaces output.args.command with a marker so the LLM sees the
//         sandbox result instead of the host shell trying to run it
//       * throws on non-zero exit so CodeBuddy aborts the tool call
//   - Other tools pass through untouched.
//
// Security notes:
//   - The plugin never embeds the API key. The spawned sandbox_exec.py uses
//     the host's CubeSandbox SDK, which reads CUBE_* from the host env.
//   - The plugin only forwards the command string; `sandbox_exec.py` runs
//     it inside a disposable MicroVM, so a malicious prompt cannot poison
//     the host filesystem beyond the session file under /tmp.
//   - If the plugin fails to spawn or the sandbox crashes, we throw so
//     CodeBuddy does not silently fall back to host execution.
//   - runInSandbox uses execFileAsync (promisified) to avoid blocking the
//     Node.js event loop during sandbox execution.

import { execFile } from "node:child_process";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";

const execFileAsync = promisify(execFile);

const PLUGIN_DIR = dirname(fileURLToPath(import.meta.url));
const EXECUTOR = join(PLUGIN_DIR, "..", "sandbox_exec.py");

/**
 * Run a shell command inside a CubeSandbox via the host-side executor.
 * Returns the captured stdout. Throws on non-zero exit so the LLM sees the
 * failure and CodeBuddy aborts the tool call.
 *
 * Uses execFileAsync (via promisify) instead of spawnSync to avoid blocking
 * the Node.js event loop. A hard timeout (5 minutes) is set as a safeguard;
 * the sandbox-side timeout in sandbox_exec.py (default 120s, max 300s) is
 * the authoritative limit for long-running commands.
 */
async function runInSandbox(command) {
  let stdout, stderr, status;

  try {
    ({ stdout, stderr, status } = await execFileAsync(
      "python3",
      [EXECUTOR, "--cmd", command],
      { encoding: "utf-8", timeout: 300000 },  // 5-minute host-side timeout
    ));
  } catch (err) {
    throw new Error(
      `cubesandbox plugin: failed to spawn sandbox_exec.py: ${err.message}`,
    );
  }

  if (status !== 0) {
    throw new Error(
      `cubesandbox plugin: sandbox command failed (exit ${status}): ${(stderr || "").trim()}`,
    );
  }
  return (stdout || "").trimEnd();
}

/** Plugin entry point — CodeBuddy invokes this once on startup. */
export const CubeSandboxBashPlugin = async () => {
  return {
    "tool.execute.before": async (input, output) => {
      // Only intercept the bash tool; every other tool (read, edit, ...)
      // runs on the host as usual.
      if (input.tool !== "bash") return;

      const command = output?.args?.command;
      if (typeof command !== "string" || command.length === 0) return;

      const stdout = await runInSandbox(command);

      // Replace the command so CodeBuddy does not run it on the host shell,
      // and surface the sandbox output as a synthetic stdout the LLM can
      // read. Subsequent tool calls in the same session reuse the cached
      // sandbox via sandbox_exec.py's session-file mechanism, so this is
      // not a cold start after the first invocation.
      output.args.command = `echo ${JSON.stringify(
        "[cubesandbox-sandbox] executed in isolated MicroVM:\n" + stdout,
      )}`;
    },
  };
};
