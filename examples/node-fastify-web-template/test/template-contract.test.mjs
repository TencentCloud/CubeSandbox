import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { test } from "node:test";

const exampleRoot = new URL("../", import.meta.url);

test("Docker entrypoint preserves tini and CMD stays aligned with the package start script", async () => {
  const [dockerfile, entrypoint, packageJsonRaw] = await Promise.all([
    readFile(new URL("Dockerfile", exampleRoot), "utf8"),
    readFile(new URL("cube-entrypoint.sh", exampleRoot), "utf8"),
    readFile(new URL("package.json", exampleRoot), "utf8"),
  ]);
  const packageJson = JSON.parse(packageJsonRaw);
  const entrypointMatch = dockerfile.match(/^ENTRYPOINT\s+(\[[^\n]+\])$/m);
  const cmdMatch = dockerfile.match(/^CMD\s+(\[[^\n]+\])$/m);

  assert.match(dockerfile, /^COPY --from=cubesandbox-base \/usr\/bin\/tini \/usr\/bin\/tini$/m);
  assert.match(
    dockerfile,
    /^COPY --chmod=0755 cube-entrypoint\.sh \/usr\/local\/bin\/cube-entrypoint\.sh$/m,
  );
  assert.ok(entrypointMatch, "Dockerfile must define a JSON-array ENTRYPOINT");
  assert.deepEqual(
    JSON.parse(entrypointMatch[1]),
    ["/usr/bin/tini", "-g", "--", "/usr/local/bin/cube-entrypoint.sh"],
  );
  assert.ok(cmdMatch, "Dockerfile must define a JSON-array CMD");
  assert.deepEqual(JSON.parse(cmdMatch[1]), packageJson.scripts.start.split(/\s+/));

  const installIndex = dockerfile.indexOf("npm ci");
  const buildIndex = dockerfile.indexOf("npm run build");
  const pruneIndex = dockerfile.indexOf("npm prune --omit=dev");
  assert.ok(installIndex >= 0 && installIndex < buildIndex);
  assert.ok(buildIndex < pruneIndex, "devDependencies must be pruned after the build");

  assert.match(entrypoint, /dash interrupts wait after running a signal trap/);
  assert.match(entrypoint, /kill -0 "\$\{pid\}"/);
});

test(".env.example points the E2B SDK at CubeAPI", async () => {
  const envExample = await readFile(new URL(".env.example", exampleRoot), "utf8");
  const apiUrl = envExample
    .split(/\r?\n/)
    .find((line) => line.startsWith("E2B_API_URL="));

  assert.equal(apiUrl, "E2B_API_URL=http://127.0.0.1:3000");
});
