import { mkdir, readFile, appendFile, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { arch, argv, platform } from "node:process";
import { pathToFileURL } from "node:url";
import Fastify, { type FastifyInstance, type FastifyReply, type FastifyRequest } from "fastify";

const defaultPort = Number.parseInt(process.env.PORT ?? "3000", 10);
const defaultStateDir = process.env.STATE_DIR ?? "/workspace/state";

type CounterFile = {
  count: number;
};

type NoteBody = {
  note: string;
};

type ServerOptions = {
  stateDir?: string;
  logger?: boolean;
};

async function ensureStateDir(stateDir: string) {
  await mkdir(stateDir, { recursive: true });
}

async function readCounter(counterPath: string): Promise<number> {
  try {
    const raw = await readFile(counterPath, "utf8");
    const parsed = JSON.parse(raw) as Partial<CounterFile>;
    return typeof parsed.count === "number" && Number.isFinite(parsed.count)
      ? parsed.count
      : 0;
  } catch (error) {
    if (error instanceof Error && "code" in error && error.code === "ENOENT") {
      return 0;
    }
    throw error;
  }
}

export function buildServer(options: ServerOptions = {}): FastifyInstance {
  const stateDir = options.stateDir ?? defaultStateDir;
  const counterPath = join(stateDir, "counter.json");
  const notesPath = join(stateDir, "notes.jsonl");
  const fastify = Fastify({
    ajv: {
      customOptions: {
        coerceTypes: false,
        removeAdditional: false,
      },
    },
    logger: options.logger ?? true,
  });

  fastify.get("/", async (_request: FastifyRequest, reply: FastifyReply) => {
    reply.type("text/html; charset=utf-8");
    return `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <title>CubeSandbox Node.js Fastify Template</title>
  </head>
  <body>
    <main>
      <h1>CubeSandbox Node.js Fastify Template</h1>
      <p>This sandbox runs CubeSandbox envd on port <strong>49983</strong>.</p>
      <p>The Fastify TypeScript web API runs on port <strong>3000</strong>.</p>
      <p>Try <code>/health</code>, <code>/api/info</code>, <code>POST /api/counter</code>, and <code>POST /api/write-note</code>.</p>
    </main>
  </body>
</html>`;
  });

  fastify.get(
    "/health",
    {
      schema: {
        response: {
          200: {
            type: "object",
            properties: {
              ok: { type: "boolean" },
            },
            required: ["ok"],
          },
        },
      },
    },
    async () => ({ ok: true }),
  );

  fastify.get(
    "/api/info",
    {
      schema: {
        response: {
          200: {
            type: "object",
            properties: {
              node: { type: "string" },
              platform: { type: "string" },
              arch: { type: "string" },
              cwd: { type: "string" },
              stateDir: { type: "string" },
            },
            required: ["node", "platform", "arch", "cwd", "stateDir"],
          },
        },
      },
    },
    async () => ({
      node: process.version,
      platform,
      arch,
      cwd: process.cwd(),
      stateDir,
    }),
  );

  fastify.post(
    "/api/counter",
    {
      schema: {
        response: {
          200: {
            type: "object",
            properties: {
              count: { type: "number" },
            },
            required: ["count"],
          },
        },
      },
    },
    async () => {
      await ensureStateDir(stateDir);
      const count = (await readCounter(counterPath)) + 1;
      await writeFile(counterPath, `${JSON.stringify({ count })}\n`, "utf8");
      return { count };
    },
  );

  fastify.post<{ Body: NoteBody }>(
    "/api/write-note",
    {
      schema: {
        body: {
          type: "object",
          properties: {
            note: { type: "string", minLength: 1 },
          },
          required: ["note"],
          additionalProperties: false,
        },
        response: {
          200: {
            type: "object",
            properties: {
              ok: { type: "boolean" },
              path: { type: "string" },
            },
            required: ["ok", "path"],
          },
        },
      },
    },
    async (request) => {
      await ensureStateDir(stateDir);
      const line = JSON.stringify({
        note: request.body.note,
        writtenAt: new Date().toISOString(),
      });
      await appendFile(notesPath, `${line}\n`, "utf8");
      return { ok: true, path: notesPath };
    },
  );

  return fastify;
}

async function main() {
  await ensureStateDir(defaultStateDir);
  const fastify = buildServer();
  await fastify.listen({ host: "0.0.0.0", port: defaultPort });
}

if (argv[1] && import.meta.url === pathToFileURL(argv[1]).href) {
  await main();
}
