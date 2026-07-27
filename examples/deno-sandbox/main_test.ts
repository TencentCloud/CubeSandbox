import { assertEquals } from "@std/assert";
import { CounterStore, createHandler } from "./main.ts";

async function withStore(
  test: (store: CounterStore, stateFile: string) => Promise<void>,
): Promise<void> {
  const directory = await Deno.makeTempDir({ prefix: "cube-deno-" });
  try {
    await test(
      new CounterStore(`${directory}/counter.json`),
      `${directory}/counter.json`,
    );
  } finally {
    await Deno.remove(directory, { recursive: true });
  }
}

async function responseJson(
  response: Response,
): Promise<Record<string, unknown>> {
  return await response.json() as Record<string, unknown>;
}

Deno.test("health reports the Deno runtime", async () => {
  await withStore(async (store) => {
    const response = await createHandler(store)(
      new Request("http://localhost/health"),
    );
    const body = await responseJson(response);

    assertEquals(response.status, 200);
    assertEquals(body.status, "ok");
    assertEquals(body.runtime, "deno");
    assertEquals(typeof body.deno, "string");
  });
});

Deno.test("counter persists across store instances", async () => {
  await withStore(async (store, stateFile) => {
    const handler = createHandler(store);
    const first = await responseJson(
      await handler(
        new Request("http://localhost/counter", { method: "POST" }),
      ),
    );
    const secondStore = new CounterStore(stateFile);
    const persisted = await responseJson(
      await createHandler(secondStore)(new Request("http://localhost/counter")),
    );

    assertEquals(first, { counter: 1 });
    assertEquals(persisted, { counter: 1 });
  });
});

Deno.test("concurrent increments are serialized", async () => {
  await withStore(async (store) => {
    const handler = createHandler(store);
    const requests = Array.from(
      { length: 20 },
      () =>
        handler(new Request("http://localhost/counter", { method: "POST" })),
    );
    const responses = await Promise.all(requests);
    const finalState = await responseJson(
      await handler(new Request("http://localhost/counter")),
    );

    assertEquals(responses.every((response) => response.status === 200), true);
    assertEquals(finalState, { counter: 20 });
  });
});

Deno.test("counter rejects unsupported methods", async () => {
  await withStore(async (store) => {
    const response = await createHandler(store)(
      new Request("http://localhost/counter", { method: "DELETE" }),
    );

    assertEquals(response.status, 405);
    assertEquals(response.headers.get("allow"), "GET, POST");
  });
});

Deno.test("unknown routes return JSON 404", async () => {
  await withStore(async (store) => {
    const response = await createHandler(store)(
      new Request("http://localhost/missing"),
    );

    assertEquals(response.status, 404);
    assertEquals(await responseJson(response), { error: "not_found" });
  });
});
