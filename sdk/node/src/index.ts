// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

export const VERSION = "0.3.0";

export {
  Sandbox,
  JUPYTER_PORT,
  type CreateOptions,
  type RunCodeOptions,
  type PauseOptions,
  type ListSnapshotsOptions,
  type NetworkOptions,
  type LifecycleOptions,
} from "./sandbox.js";

export { Config, resolveConfig, type ConfigOptions } from "./config.js";

export {
  Execution,
  Result,
  Logs,
  ExecutionError,
  OutputMessage,
  SnapshotInfo,
} from "./models.js";

export {
  CubeSandboxError,
  SandboxNotFoundError,
  TemplateNotFoundError,
  AuthenticationError,
  ApiError,
  FilesystemNotFoundError,
  PartialWriteError,
} from "./exceptions.js";

export {
  Commands,
  ENVD_PORT,
  DEFAULT_ENVD_USER,
  type CommandResult,
  type CommandOptions,
} from "./commands.js";

export {
  Filesystem,
  Watcher,
  type FileEntry,
  type WriteEntry,
  type WatchEvent,
} from "./filesystem.js";

export {
  Pty,
  PtyHandle,
  type PtySize,
  type PtyOutput,
  type PtyCreateOptions,
  type PtyConnectOptions,
} from "./pty.js";

export {
  Template,
  TemplateInfo,
  TemplateBuild,
  type TemplateBuildOptions,
} from "./template.js";

export {
  type Rule,
  type Match,
  type Action,
  type Inject,
  type Scheme,
  type Method,
  type AuditLevel,
  type NetworkRules,
  type E2BPerHostRules,
  type E2BTransformEntry,
} from "./policy.js";

export { type RunCodeCallbacks } from "./stream.js";
