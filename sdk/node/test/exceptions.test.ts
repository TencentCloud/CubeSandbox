// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from "vitest";

import {
  ApiError,
  AuthenticationError,
  CubeSandboxError,
  FilesystemNotFoundError,
  PartialWriteError,
  SandboxNotFoundError,
  TemplateNotFoundError,
} from "../src/exceptions.js";

describe("exception hierarchy", () => {
  const cases: Array<[new (message: string, statusCode?: number) => CubeSandboxError, string]> = [
    [SandboxNotFoundError, "SandboxNotFoundError"],
    [TemplateNotFoundError, "TemplateNotFoundError"],
    [AuthenticationError, "AuthenticationError"],
    [ApiError, "ApiError"],
    [FilesystemNotFoundError, "FilesystemNotFoundError"],
  ];

  for (const [Ctor, name] of cases) {
    it(`${name} extends CubeSandboxError and Error`, () => {
      const err = new Ctor("boom", 404);
      expect(err).toBeInstanceOf(CubeSandboxError);
      expect(err).toBeInstanceOf(Error);
      expect(err).toBeInstanceOf(Ctor);
      expect(err.name).toBe(name);
      expect(err.message).toBe("boom");
      expect(err.statusCode).toBe(404);
    });
  }

  it("CubeSandboxError carries an optional statusCode", () => {
    expect(new CubeSandboxError("x").statusCode).toBeUndefined();
    expect(new CubeSandboxError("x", 500).statusCode).toBe(500);
  });

  it("distinguishes sibling error subclasses", () => {
    const err = new AuthenticationError("nope", 401);
    expect(err).not.toBeInstanceOf(ApiError);
    expect(err).not.toBeInstanceOf(SandboxNotFoundError);
  });
});

describe("PartialWriteError", () => {
  it("records how many files were written before failure", () => {
    const err = new PartialWriteError("failed at /tmp/b.txt", 2);
    expect(err).toBeInstanceOf(CubeSandboxError);
    expect(err).toBeInstanceOf(PartialWriteError);
    expect(err.name).toBe("PartialWriteError");
    expect(err.written).toBe(2);
    expect(err.message).toContain("/tmp/b.txt");
  });
});
