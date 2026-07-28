import { dirname } from "@std/path";

const JSON_HEADERS = {
  "cache-control": "no-store",
  "content-type": "application/json; charset=utf-8",
};

export interface CounterState {
  counter: number;
}

/** A small file-backed store whose writes are serialized within the process. */
export class CounterStore {
  readonly #stateFile: string;
  #pending: Promise<void> = Promise.resolve();

  constructor(stateFile: string) {
    this.#stateFile = stateFile;
  }

  async read(): Promise<CounterState> {
    let serialized: string;
    try {
      serialized = await Deno.readTextFile(this.#stateFile);
    } catch (error) {
      if (error instanceof Deno.errors.NotFound) {
        return { counter: 0 };
      }
      throw error;
    }

    let parsed: unknown;
    try {
      parsed = JSON.parse(serialized);
    } catch (error) {
      throw new Error(`Invalid JSON counter state in ${this.#stateFile}`, {
        cause: error,
      });
    }

    const counter = typeof parsed === "object" && parsed !== null &&
        "counter" in parsed
      ? parsed.counter
      : undefined;
    if (
      typeof counter !== "number" ||
      !Number.isSafeInteger(counter) || counter < 0
    ) {
      throw new Error(`Invalid counter state in ${this.#stateFile}`);
    }
    return { counter };
  }

  /**
   * Persist one increment. A failed write rejects this call and is not retried;
   * later calls continue from the last successfully persisted state.
   */
  increment(): Promise<CounterState> {
    const operation = this.#pending.then(async () => {
      const current = await this.read();
      const next = { counter: current.counter + 1 };
      await Deno.mkdir(dirname(this.#stateFile), { recursive: true });
      const temporaryFile = `${this.#stateFile}.tmp`;
      try {
        await Deno.writeTextFile(
          temporaryFile,
          `${JSON.stringify(next, null, 2)}\n`,
        );
        await Deno.rename(temporaryFile, this.#stateFile);
      } catch (error) {
        try {
          await Deno.remove(temporaryFile);
        } catch (cleanupError) {
          if (!(cleanupError instanceof Deno.errors.NotFound)) {
            console.warn(
              `Failed to remove temporary counter state ${temporaryFile}`,
              cleanupError,
            );
          }
        }
        throw error;
      }
      return next;
    });

    // Promise callbacks report synchronous throws as rejections, so installing
    // this recovery handler always completes before the operation is returned.
    // A failed operation must not permanently poison the queue.
    this.#pending = operation.then(() => undefined, () => undefined);
    return operation;
  }
}

function json(data: unknown, init: ResponseInit = {}): Response {
  return new Response(JSON.stringify(data), {
    ...init,
    headers: { ...JSON_HEADERS, ...init.headers },
  });
}

export function createHandler(
  store: CounterStore,
): (request: Request) => Promise<Response> {
  return async (request: Request): Promise<Response> => {
    const { pathname } = new URL(request.url);

    try {
      if (pathname === "/health" && request.method === "GET") {
        return json({
          status: "ok",
          runtime: "deno",
          deno: Deno.version.deno,
          typescript: Deno.version.typescript,
        });
      }

      if (pathname === "/counter") {
        if (request.method === "GET") {
          return json(await store.read());
        }
        if (request.method === "POST") {
          return json(await store.increment());
        }
        return json(
          { error: "method_not_allowed" },
          { status: 405, headers: { allow: "GET, POST" } },
        );
      }

      return json({ error: "not_found" }, { status: 404 });
    } catch (error) {
      console.error("request failed", error);
      return json({ error: "internal_server_error" }, { status: 500 });
    }
  };
}

if (import.meta.main) {
  const stateFile = "/workspace/deno-app/data/counter.json";
  const handler = createHandler(new CounterStore(stateFile));
  console.log("Deno demo listening on http://0.0.0.0:8000");
  Deno.serve({ hostname: "0.0.0.0", port: 8000 }, handler);
}
