// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach } from "vitest";

const CUBE_ENV_KEYS = [
  "CUBE_API_URL",
  "CUBE_API_KEY",
  "E2B_API_KEY",
  "CUBE_TEMPLATE_ID",
  "CUBE_PROXY_NODE_IP",
  "CUBE_PROXY_PORT_HTTP",
  "CUBE_PROXY_SCHEME",
  "CUBE_SANDBOX_DOMAIN",
] as const;

/**
 * Isolate a test suite from any ambient ``CUBE_*`` configuration so unit tests
 * stay hermetic even when the shell or CI has them set — e.g. when running the
 * gated live integration test in the same `npm test` invocation. Registers
 * `beforeEach`/`afterEach` hooks that clear the vars before each test and
 * restore the originals afterwards.
 */
export function stubCubeEnv(): void {
  let saved: Record<string, string | undefined> = {};
  beforeEach(() => {
    saved = {};
    for (const key of CUBE_ENV_KEYS) {
      saved[key] = process.env[key];
      delete process.env[key];
    }
  });
  afterEach(() => {
    for (const key of CUBE_ENV_KEYS) {
      const value = saved[key];
      if (value === undefined) {
        delete process.env[key];
      } else {
        process.env[key] = value;
      }
    }
  });
}
