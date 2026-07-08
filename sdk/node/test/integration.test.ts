// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// End-to-end tests against a live CubeAPI. Skipped unless CUBE_RUN_INTEGRATION="1".
//
// Required environment (read by Config):
//   CUBE_API_URL, CUBE_TEMPLATE_ID, CUBE_PROXY_NODE_IP, CUBE_PROXY_PORT_HTTP,
//   CUBE_SANDBOX_DOMAIN
//
// Run with, e.g.:
//   CUBE_RUN_INTEGRATION=1 CUBE_API_URL=... CUBE_TEMPLATE_ID=... npm test

import { describe, expect, it } from "vitest";

import { Config } from "../src/config.js";
import { Sandbox } from "../src/index.js";

const RUN_INTEGRATION = process.env.CUBE_RUN_INTEGRATION === "1";

describe.skipIf(!RUN_INTEGRATION)("integration (live CubeAPI)", () => {
  it(
    "creates a sandbox, runs code and commands, writes/reads a file, then kills it",
    async () => {
      const config = new Config();
      const sb = await Sandbox.create({ config });
      try {
        const execution = await sb.runCode("1+1");
        expect(execution.text).toBe("2");

        const cmd = await sb.commands.run("echo hello");
        expect(cmd.exitCode).toBe(0);
        expect(cmd.stdout).toContain("hello");

        await sb.files.write("/tmp/cube-sdk-e2e.txt", "cube");
        expect(await sb.files.read("/tmp/cube-sdk-e2e.txt")).toBe("cube");
      } finally {
        await sb.kill();
        sb.close();
      }
    },
    60_000,
  );
});
