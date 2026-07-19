import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { test } from "node:test";

const entrypointPath = new URL("../cube-entrypoint.sh", import.meta.url).pathname;

test(
  "entrypoint waits for a user process after TERM interrupts dash wait",
  { skip: process.platform === "win32" },
  async (t) => {
    const workDir = await mkdtemp(path.join(tmpdir(), "cube-entrypoint-test-"));
    const userCommand = path.join(workDir, "slow-shutdown.sh");
    await writeFile(
      userCommand,
      [
        "#!/bin/sh",
        "trap 'sleep 0.4; exit 0' TERM",
        "echo READY",
        "while :; do sleep 0.1; done",
        "",
      ].join("\n"),
      { encoding: "utf8", mode: 0o755 },
    );

    const child = spawn("/bin/sh", [entrypointPath, userCommand], {
      detached: true,
      env: {
        ...process.env,
        ENVD_BIN: "/bin/true",
        ENVD_LOG_FILE: "-",
      },
      stdio: ["ignore", "pipe", "pipe"],
    });

    t.after(async () => {
      try {
        process.kill(-child.pid, "SIGKILL");
      } catch (error) {
        if (error?.code !== "ESRCH") {
          throw error;
        }
      }
      await rm(workDir, { recursive: true, force: true });
    });

    await new Promise((resolve, reject) => {
      let stdout = "";
      const timeout = setTimeout(() => reject(new Error("user command did not become ready")), 5_000);
      child.stdout.on("data", (chunk) => {
        stdout += chunk;
        if (stdout.includes("READY")) {
          clearTimeout(timeout);
          resolve();
        }
      });
      child.once("error", reject);
    });

    const startedAt = Date.now();
    child.kill("SIGTERM");
    const result = await new Promise((resolve, reject) => {
      const timeout = setTimeout(() => {
        try {
          process.kill(-child.pid, "SIGKILL");
        } catch {
          // The process may have exited between the timeout and cleanup.
        }
        reject(new Error("entrypoint did not exit after the user command drained"));
      }, 5_000);
      child.once("exit", (code, signal) => {
        clearTimeout(timeout);
        resolve({ code, signal });
      });
      child.once("error", reject);
    });

    assert.deepEqual(result, { code: 0, signal: null });
    assert.ok(
      Date.now() - startedAt >= 300,
      "entrypoint exited before the user process completed its shutdown delay",
    );
  },
);
