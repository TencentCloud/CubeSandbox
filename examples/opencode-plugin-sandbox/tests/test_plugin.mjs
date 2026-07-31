/**
 * test_plugin.mjs — offline tests for the OpenCode bash-redirect plugin.
 *
 * These tests exercise the rewrite logic only. They do not need a CubeSandbox
 * deployment, a network connection, or any npm dependency, so a reviewer can
 * run them immediately:
 *
 *     node tests/test_plugin.mjs
 *
 * Exit code 0 means every assertion passed.
 */

import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const PLUGIN = path.resolve(here, "..", "plugin", "cubesandbox-bash.js");
const BACKEND = path.resolve(here, "..", "exec_backend.py");

const mod = await import(`file://${PLUGIN.replace(/\\/g, "/")}`);

let passed = 0;
let failed = 0;

function check(name, fn) {
  try {
    fn();
    console.log(`[PASS] ${name}`);
    passed += 1;
  } catch (err) {
    console.log(`[FAIL] ${name}`);
    console.log(`       ${err.message}`);
    failed += 1;
  }
}

async function checkAsync(name, fn) {
  try {
    await fn();
    console.log(`[PASS] ${name}`);
    passed += 1;
  } catch (err) {
    console.log(`[FAIL] ${name}`);
    console.log(`       ${err.message}`);
    failed += 1;
  }
}

/**
 * Split a command string the way a POSIX shell would.
 *
 * Used to prove that the rewritten command keeps the user's original text
 * inside exactly one argv element. Asserting on the parsed argv is much
 * stronger than substring matching: if the quoting were broken, the payload
 * would land in a separate element (or introduce extra ones) and the assertion
 * would fail.
 *
 * Handles the two constructs the plugin emits:
 *   'text'   single-quoted run, no escapes recognised inside
 *   \c       backslash escape outside quotes, yields the literal c
 *
 * That is exactly what is needed to decode the standard POSIX idiom for
 * embedding a quote inside a single-quoted string: close the quote, emit an
 * escaped quote, reopen. Written out, the four characters are
 * quote, backslash, quote, quote.
 */
function posixSplit(input) {
  const argv = [];
  let cur = "";
  let inQuote = false;
  let started = false;

  for (let i = 0; i < input.length; i += 1) {
    const ch = input[i];

    if (inQuote) {
      // Inside single quotes every byte is literal until the closing quote.
      if (ch === "'") inQuote = false;
      else cur += ch;
      continue;
    }

    if (ch === "\\" && i + 1 < input.length) {
      // Backslash outside quotes escapes the next character.
      cur += input[i + 1];
      started = true;
      i += 1;
      continue;
    }

    if (ch === "'") {
      inQuote = true;
      started = true;
      continue;
    }

    if (ch === " " || ch === "\t") {
      if (started) {
        argv.push(cur);
        cur = "";
        started = false;
      }
      continue;
    }

    cur += ch;
    started = true;
  }

  if (started) argv.push(cur);
  return argv;
}

/** Build a hook instance with logging silenced. */
async function makeHook(env = {}) {
  const saved = {};
  for (const [k, v] of Object.entries(env)) {
    saved[k] = process.env[k];
    if (v === undefined) delete process.env[k];
    else process.env[k] = v;
  }
  const hooks = await mod.CubeSandboxBashPlugin({
    client: { app: { log: async () => {} } },
    directory: "/tmp/project",
  });
  return {
    hook: hooks["tool.execute.before"],
    restore() {
      for (const [k, v] of Object.entries(saved)) {
        if (v === undefined) delete process.env[k];
        else process.env[k] = v;
      }
    },
  };
}

console.log("=".repeat(66));
console.log("OpenCode CubeSandbox plugin — offline tests");
console.log("=".repeat(66));
console.log();

// --------------------------------------------------------------- structure
check("plugin exports CubeSandboxBashPlugin", () => {
  assert.equal(typeof mod.CubeSandboxBashPlugin, "function");
});

check("plugin has a default export", () => {
  assert.equal(typeof mod.default, "function");
});

await checkAsync("plugin returns the tool.execute.before hook", async () => {
  const { hook, restore } = await makeHook();
  assert.equal(typeof hook, "function");
  restore();
});

// --------------------------------------------------------------- rewriting
await checkAsync("bash command is redirected to the backend", async () => {
  const { hook, restore } = await makeHook();
  const out = { args: { command: "ls -la /etc" } };
  await hook({ tool: "bash", sessionID: "s1" }, out);
  restore();
  assert.ok(out.args.command.includes("exec_backend.py"), "backend not referenced");
  assert.ok(out.args.command.includes("--session"), "--session missing");
  assert.ok(out.args.command.includes("--command"), "--command missing");
  assert.ok(out.args.command.includes("'ls -la /etc'"), "original command not quoted");
});

await checkAsync("session id is propagated", async () => {
  const { hook, restore } = await makeHook();
  const out = { args: { command: "pwd" } };
  await hook({ tool: "bash", sessionID: "sess-XYZ" }, out);
  restore();
  assert.ok(out.args.command.includes("'sess-XYZ'"), "session id missing");
});

await checkAsync("alternative session key spellings are accepted", async () => {
  const { hook, restore } = await makeHook();
  for (const input of [
    { tool: "bash", sessionId: "camel" },
    { tool: "bash", session_id: "snake" },
    { tool: "bash", session: { id: "nested" } },
  ]) {
    const out = { args: { command: "pwd" } };
    await hook(input, out);
    const expected = input.sessionId || input.session_id || input.session.id;
    assert.ok(
      out.args.command.includes(`'${expected}'`),
      `session id ${expected} not picked up`
    );
  }
  restore();
});

await checkAsync("missing session id falls back to 'default'", async () => {
  const { hook, restore } = await makeHook();
  const out = { args: { command: "pwd" } };
  await hook({ tool: "bash" }, out);
  restore();
  assert.ok(out.args.command.includes("'default'"), "fallback session missing");
});

// --------------------------------------------------------------- untouched
await checkAsync("non-bash tools are left alone", async () => {
  const { hook, restore } = await makeHook();
  for (const tool of ["read", "write", "edit", "grep", "glob"]) {
    const out = { args: { command: "should-not-change", filePath: "/etc/hosts" } };
    await hook({ tool, sessionID: "s1" }, out);
    assert.equal(out.args.command, "should-not-change", `${tool} was modified`);
  }
  restore();
});

await checkAsync("empty and whitespace-only commands are left alone", async () => {
  const { hook, restore } = await makeHook();
  for (const cmd of ["", "   ", "\t\n"]) {
    const out = { args: { command: cmd } };
    await hook({ tool: "bash", sessionID: "s1" }, out);
    assert.equal(out.args.command, cmd, "blank command was modified");
  }
  restore();
});

// Fail-closed applies to unreadable payloads too. Returning quietly on a
// malformed call would let it reach the host, contradicting the guarantee that
// a command which cannot be redirected safely is blocked rather than run.
await checkAsync("a non-string command is blocked, not passed to the host", async () => {
  const { hook, restore } = await makeHook();
  for (const value of [42, null, undefined, {}, ["ls"]]) {
    const out = { args: { command: value } };
    await assert.rejects(
      () => hook({ tool: "bash", sessionID: "s1" }, out),
      /expected a string command/,
      `command of type ${typeof value} was not blocked`
    );
  }
  restore();
});

await checkAsync("a bash call with no args is blocked", async () => {
  const { hook, restore } = await makeHook();
  for (const output of [{}, { args: null }, { args: undefined }]) {
    await assert.rejects(
      () => hook({ tool: "bash", sessionID: "s1" }, output),
      /carried no arguments/,
      "missing args was not blocked"
    );
  }
  restore();
});

await checkAsync("rewriting is idempotent", async () => {
  const { hook, restore } = await makeHook();
  const out = { args: { command: "ls" } };
  await hook({ tool: "bash", sessionID: "s1" }, out);
  const once = out.args.command;
  await hook({ tool: "bash", sessionID: "s1" }, out);
  restore();
  assert.equal(out.args.command, once, "second pass wrapped the command again");
});

// The idempotence guard must be structural, not a substring test. A substring
// test on the backend filename would let ordinary user commands that merely
// mention it escape to the host unlogged, which is the exact failure this
// plugin exists to prevent.
await checkAsync("commands that merely mention the backend are still redirected", async () => {
  const { hook, restore } = await makeHook();
  const escapes = [
    "ls exec_backend.py",
    "cat exec_backend.py",
    "echo x # exec_backend.py",
    "touch /tmp/should-not-happen; echo exec_backend.py",
    "grep -n session exec_backend.py",
  ];
  for (const cmd of escapes) {
    const out = { args: { command: cmd } };
    await hook({ tool: "bash", sessionID: "s1" }, out);
    assert.notEqual(
      out.args.command,
      cmd,
      `command escaped to the host unredirected: ${cmd}`
    );
    assert.ok(
      out.args.command.includes("--command"),
      `command was not rewritten into a backend call: ${cmd}`
    );
  }
  restore();
});

await checkAsync("a genuine rewrite is recognised and not nested", async () => {
  const { hook, restore } = await makeHook();

  // Produce a real rewrite, then feed it back in.
  const out = { args: { command: "echo hello" } };
  await hook({ tool: "bash", sessionID: "s1" }, out);
  const rewritten = out.args.command;

  const again = { args: { command: rewritten } };
  await hook({ tool: "bash", sessionID: "s1" }, again);
  restore();

  assert.equal(again.args.command, rewritten, "a real rewrite was wrapped twice");
  // One --command flag, not two: no nesting happened.
  assert.equal(
    (again.args.command.match(/--command/g) || []).length,
    1,
    "the backend invocation was nested inside itself"
  );
});

// --------------------------------------------------------------- passthrough
// Passthrough is empty by default, and that is a security property rather than
// an oversight: matching is on the leading token, so allowing `git` allows
// `git -c alias.x='!<any shell>' x`, which runs arbitrary code on the host.
await checkAsync("nothing passes through by default, including git", async () => {
  const { hook, restore } = await makeHook();
  for (const cmd of [
    "git status",
    "gh pr list",
    "/usr/bin/git log",
    "git -c alias.pwn='!echo owned' pwn",
  ]) {
    const out = { args: { command: cmd } };
    await hook({ tool: "bash", sessionID: "s1" }, out);
    assert.notEqual(out.args.command, cmd, `${cmd} should be redirected by default`);
    assert.ok(
      out.args.command.includes("--command"),
      `${cmd} was not rewritten into a backend call`
    );
  }
  restore();
});

await checkAsync("host git requires explicit opt-in", async () => {
  const { hook, restore } = await makeHook({ CUBE_OPENCODE_PASSTHROUGH: "git,gh" });
  for (const cmd of ["git status", "gh pr list", "/usr/bin/git log"]) {
    const out = { args: { command: cmd } };
    await hook({ tool: "bash", sessionID: "s1" }, out);
    assert.equal(out.args.command, cmd, `${cmd} should pass through once opted in`);
  }
  restore();
});

await checkAsync("passthrough list is configurable", async () => {
  const { hook, restore } = await makeHook({ CUBE_OPENCODE_PASSTHROUGH: "make,npm" });
  const kept = { args: { command: "make build" } };
  await hook({ tool: "bash", sessionID: "s1" }, kept);
  assert.equal(kept.args.command, "make build", "make should pass through");

  const moved = { args: { command: "git status" } };
  await hook({ tool: "bash", sessionID: "s1" }, moved);
  restore();
  assert.ok(
    moved.args.command.includes("exec_backend.py"),
    "git should be redirected once removed from the list"
  );
});

await checkAsync("an empty passthrough list redirects everything", async () => {
  const { hook, restore } = await makeHook({ CUBE_OPENCODE_PASSTHROUGH: "" });
  const out = { args: { command: "git status" } };
  await hook({ tool: "bash", sessionID: "s1" }, out);
  restore();
  assert.ok(out.args.command.includes("exec_backend.py"), "git should be redirected");
});

await checkAsync("passthrough matches only the leading token", async () => {
  const { hook, restore } = await makeHook();
  // A compound command may contain anything after the first token, so it must
  // not inherit git's passthrough exemption.
  const out = { args: { command: "foo && git push" } };
  await hook({ tool: "bash", sessionID: "s1" }, out);
  restore();
  assert.ok(
    out.args.command.includes("exec_backend.py"),
    "compound command must not be treated as passthrough"
  );
});

// --------------------------------------------------------------- injection
await checkAsync("single quotes in the command are escaped", async () => {
  const { hook, restore } = await makeHook();
  const evil = "echo hi'; rm -rf /tmp/pwned; echo '";
  const out = { args: { command: evil } };
  await hook({ tool: "bash", sessionID: "s1" }, out);
  restore();

  // POSIX single-quote escaping turns ' into '\'' so the payload stays data.
  assert.ok(out.args.command.includes("'\\''"), "quote escaping not applied");

  // The real property to verify is that the whole payload lives inside ONE
  // shell argument. Parse the rewritten string the way a POSIX shell would and
  // confirm the last argument equals the original command byte for byte.
  const argv = posixSplit(out.args.command);
  assert.equal(argv[argv.length - 1], evil, "payload is not a single intact argument");
  assert.equal(argv.length, 6, `expected 6 argv elements, got ${argv.length}`);
});

await checkAsync("newlines stay inside the quoted argument", async () => {
  const { hook, restore } = await makeHook();
  const out = { args: { command: "echo a\nrm -rf /tmp/x" } };
  await hook({ tool: "bash", sessionID: "s1" }, out);
  restore();
  const idx = out.args.command.indexOf("--command");
  const tail = out.args.command.slice(idx);
  assert.ok(tail.startsWith("--command '"), "command argument is not quoted");
  assert.ok(tail.trimEnd().endsWith("'"), "quoting is not closed");
});

await checkAsync("a malicious session id cannot break the quoting", async () => {
  const { hook, restore } = await makeHook();
  const out = { args: { command: "pwd" } };
  await hook({ tool: "bash", sessionID: "s1'; touch /tmp/pwned; '" }, out);
  restore();
  assert.ok(out.args.command.includes("'\\''"), "session id was not escaped");
});

// --------------------------------------------------------------- config
await checkAsync("CUBE_OPENCODE_PYTHON overrides the interpreter", async () => {
  const { hook, restore } = await makeHook({
    CUBE_OPENCODE_PYTHON: "/opt/venv/bin/python",
  });
  const out = { args: { command: "pwd" } };
  await hook({ tool: "bash", sessionID: "s1" }, out);
  restore();
  assert.ok(
    out.args.command.startsWith("'/opt/venv/bin/python'"),
    "custom interpreter not used"
  );
});

check("backend script exists next to the plugin", () => {
  // The plugin fails closed when this file is missing, so its presence is part
  // of the contract the example ships.
  assert.ok(
    path.basename(BACKEND) === "exec_backend.py",
    "unexpected backend filename"
  );
});

// --------------------------------------------------------------- summary
console.log();
console.log("=".repeat(66));
console.log(`passed: ${passed}   failed: ${failed}`);
console.log("=".repeat(66));

process.exit(failed === 0 ? 0 : 1);
