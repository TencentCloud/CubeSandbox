import assert from "node:assert/strict";
import { mkdtemp, readFile, rm, stat, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { test } from "node:test";

import { buildServer } from "../dist/server.js";

async function makeApp(t) {
  const stateDir = await mkdtemp(path.join(tmpdir(), "cube-fastify-state-"));
  const app = buildServer({ stateDir, logger: false });
  t.after(async () => {
    await app.close();
    await rm(stateDir, { recursive: true, force: true });
  });
  return { app, stateDir };
}

test("GET / returns HTML landing page", async (t) => {
  const { app } = await makeApp(t);

  const response = await app.inject({ method: "GET", url: "/" });

  assert.equal(response.statusCode, 200);
  assert.match(response.headers["content-type"], /text\/html/);
  assert.match(response.body, /CubeSandbox Node\.js Fastify Template/);
});

test("counter creates missing state directory and persists increments", async (t) => {
  const { app, stateDir } = await makeApp(t);

  const first = await app.inject({ method: "POST", url: "/api/counter" });
  assert.equal(first.statusCode, 200);
  assert.deepEqual(first.json(), { count: 1 });

  const second = await app.inject({ method: "POST", url: "/api/counter" });
  assert.equal(second.statusCode, 200);
  assert.deepEqual(second.json(), { count: 2 });

  const raw = await readFile(path.join(stateDir, "counter.json"), "utf8");
  assert.equal(raw, '{"count":2}\n');
});

test("counter serializes concurrent increments", async (t) => {
  const { app, stateDir } = await makeApp(t);

  const responses = await Promise.all(
    Array.from({ length: 10 }, () => app.inject({ method: "POST", url: "/api/counter" })),
  );

  const counts = responses.map((response) => {
    assert.equal(response.statusCode, 200);
    return response.json().count;
  });
  assert.deepEqual(counts.toSorted((left, right) => left - right), [1, 2, 3, 4, 5, 6, 7, 8, 9, 10]);
  assert.equal(await readFile(path.join(stateDir, "counter.json"), "utf8"), '{"count":10}\n');
});

test("counter treats invalid count values as zero", async (t) => {
  const cases = [
    ["missing count", "{}"],
    ["wrong type", "{\"count\":\"7\"}"],
    ["null", "{\"count\":null}"],
    ["non-finite", "{\"count\":1e999}"],
  ];

  for (const [name, raw] of cases) {
    await t.test(name, async (t) => {
      const { app, stateDir } = await makeApp(t);
      await writeFile(path.join(stateDir, "counter.json"), raw, "utf8");

      const response = await app.inject({ method: "POST", url: "/api/counter" });

      assert.equal(response.statusCode, 200);
      assert.deepEqual(response.json(), { count: 1 });
      assert.equal(await readFile(path.join(stateDir, "counter.json"), "utf8"), '{"count":1}\n');
    });
  }
});

test("counter does not silently overwrite a corrupted counter file", async (t) => {
  const { app, stateDir } = await makeApp(t);
  await writeFile(path.join(stateDir, "counter.json"), "{not-json", "utf8");

  const response = await app.inject({ method: "POST", url: "/api/counter" });

  assert.equal(response.statusCode, 500);
  assert.equal(await readFile(path.join(stateDir, "counter.json"), "utf8"), "{not-json");
});

test("write-note rejects invalid bodies without creating notes.jsonl", async (t) => {
  const { app, stateDir } = await makeApp(t);
  const invalidBodies = [
    {},
    { note: "" },
    { note: "ok", unexpected: true },
    { note: 42 },
    { note: "x".repeat(10_001) },
  ];

  for (const body of invalidBodies) {
    const response = await app.inject({
      method: "POST",
      url: "/api/write-note",
      payload: body,
    });
    assert.equal(response.statusCode, 400, `body ${JSON.stringify(body)} should fail`);
  }

  await assert.rejects(
    () => stat(path.join(stateDir, "notes.jsonl")),
    (error) => error?.code === "ENOENT",
  );
});

test("write-note rejects malformed JSON sent with application/json", async (t) => {
  const { app, stateDir } = await makeApp(t);

  const response = await app.inject({
    method: "POST",
    url: "/api/write-note",
    headers: { "content-type": "application/json" },
    payload: "{\"note\":",
  });

  assert.equal(response.statusCode, 400);
  await assert.rejects(
    () => stat(path.join(stateDir, "notes.jsonl")),
    (error) => error?.code === "ENOENT",
  );
});

test("write-note appends json lines only after schema validation passes", async (t) => {
  const { app, stateDir } = await makeApp(t);

  const response = await app.inject({
    method: "POST",
    url: "/api/write-note",
    payload: { note: "keep me" },
  });

  assert.equal(response.statusCode, 200);
  assert.deepEqual(response.json(), { ok: true, path: path.join(stateDir, "notes.jsonl") });

  const lines = (await readFile(path.join(stateDir, "notes.jsonl"), "utf8")).trim().split("\n");
  assert.equal(lines.length, 1);
  const saved = JSON.parse(lines[0]);
  assert.equal(saved.note, "keep me");
  assert.match(saved.writtenAt, /^\d{4}-\d{2}-\d{2}T/);
});

test("real listener serves health and info on an ephemeral port", async (t) => {
  const { app, stateDir } = await makeApp(t);
  await app.listen({ host: "127.0.0.1", port: 0 });
  const address = app.server.address();
  assert.equal(typeof address, "object");
  assert.ok(address);

  const baseUrl = `http://127.0.0.1:${address.port}`;
  const health = await fetch(`${baseUrl}/health`);
  assert.equal(health.status, 200);
  assert.deepEqual(await health.json(), { ok: true });

  const info = await fetch(`${baseUrl}/api/info`);
  assert.equal(info.status, 200);
  const payload = await info.json();
  assert.equal(payload.stateDir, stateDir);
  assert.equal(payload.platform, process.platform);
  assert.equal(payload.arch, process.arch);
  assert.equal(payload.node, process.version);
  assert.equal(payload.cwd, process.cwd());
});
