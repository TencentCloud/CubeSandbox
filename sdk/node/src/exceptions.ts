// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

/** Base class for all CubeSandbox SDK errors. */
export class CubeSandboxError extends Error {
  readonly statusCode?: number;

  constructor(message: string, statusCode?: number) {
    super(message);
    this.name = new.target.name;
    this.statusCode = statusCode;
    Object.setPrototypeOf(this, new.target.prototype);
  }
}

/** Raised when a sandbox is not found (HTTP 404). */
export class SandboxNotFoundError extends CubeSandboxError {}

/** Raised when a template is not found (HTTP 404). */
export class TemplateNotFoundError extends CubeSandboxError {}

/** Raised on authentication/authorization failure (HTTP 401/403). */
export class AuthenticationError extends CubeSandboxError {}

/** Raised on an unexpected backend error (HTTP 4xx/5xx). */
export class ApiError extends CubeSandboxError {}

/** Raised when a filesystem path is not found. */
export class FilesystemNotFoundError extends CubeSandboxError {}

/**
 * Raised when {@link Filesystem.writeFiles} fails partway through.
 *
 * ``written`` is the number of files successfully written before the failure.
 */
export class PartialWriteError extends CubeSandboxError {
  readonly written: number;

  constructor(message: string, written: number) {
    super(message);
    this.written = written;
  }
}
