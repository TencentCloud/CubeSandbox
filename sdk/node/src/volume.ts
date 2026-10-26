// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

/**
 * CubeSandbox persistent-volume management.
 *
 * e2b-compatible: the {@link Volume} static methods mirror the e2b SDK (and
 * the `cubesandbox` Python SDK's `Volume` class). Volumes survive sandbox
 * lifecycles; mount them at creation via `Sandbox.create({ volumeMounts })`.
 */

import { Config, type ConfigOptions, resolveConfig } from "./config.js";
import {
  ApiError,
  AuthenticationError,
  VolumeInUseError,
  VolumeNotFoundError,
} from "./exceptions.js";
import { controlFetch } from "./transport.js";

/**
 * Mirrors CubeAPI's `MAX_VOLUME_NAME_LEN` and the `^[a-zA-Z0-9_-]+$` rule
 * enforced in `CubeAPI/src/handlers/volumes.rs`. Validating client-side turns
 * an opaque HTTP 400 into a clean error at the call site.
 */
export const MAX_VOLUME_NAME_LEN = 128;
const VOLUME_NAME_RE = /^[a-zA-Z0-9_-]+$/;

const INSPECT = Symbol.for("nodejs.util.inspect.custom");

function maskToken(token: string): string {
  return token ? "***" : "";
}

/** A CubeSandbox persistent volume descriptor. */
export class VolumeInfo {
  /** Stable identifier (equals ``name`` or an auto-generated UUID). */
  readonly volumeId: string;
  /** Human-readable display name. */
  readonly name: string;
  /**
   * Optional auth token issued by the volume plugin. Empty when the plugin
   * does not issue one, and always empty for entries returned by
   * {@link Volume.list} (tokens are only surfaced on create / get-single).
   * It is a data-plane credential: `toString`/`console.log` mask it, so read
   * the field directly when the actual value is needed.
   */
  readonly token: string;

  constructor(volumeId: string, name: string, token = "") {
    this.volumeId = volumeId;
    this.name = name;
    this.token = token;
  }

  static fromDict(data: Record<string, any>): VolumeInfo {
    return new VolumeInfo(
      data.volumeID ?? data.volume_id ?? "",
      data.name ?? "",
      data.token ?? "",
    );
  }

  /** Masks the token so the credential never leaks into logs or error text. */
  toString(): string {
    return `VolumeInfo(volumeId=${JSON.stringify(this.volumeId)}, name=${JSON.stringify(
      this.name,
    )}, token=${JSON.stringify(maskToken(this.token))})`;
  }

  /** `console.log` / `util.inspect` render the masked form as well. */
  [INSPECT](): string {
    return this.toString();
  }
}

/**
 * CubeSandbox mount options for one Volume attachment.
 *
 * Plain {@link Volume}, {@link VolumeInfo} and volume-ID string values remain
 * the e2b-compatible shorthand in ``volumeMounts``. Wrap a value only when
 * Cube-specific attachment options such as ``readOnly`` are needed.
 */
export class VolumeMount {
  readonly volume: Volume | VolumeInfo | string;
  readonly readOnly: boolean;

  constructor(volume: Volume | VolumeInfo | string, options: { readOnly?: boolean } = {}) {
    this.volume = volume;
    this.readOnly = options.readOnly ?? false;
  }
}

/**
 * ``Sandbox.create({ volumeMounts })`` argument — an e2b-compatible mapping of
 * mount path → volume, with an optional {@link VolumeMount} wrapper for
 * attachment-specific options.
 */
export type VolumeMountsArg = Record<string, Volume | VolumeInfo | string | VolumeMount>;

function validateName(name: string): void {
  if (!name) {
    return;
  }
  if (name.length > MAX_VOLUME_NAME_LEN) {
    throw new Error(
      `volume name must be at most ${MAX_VOLUME_NAME_LEN} characters, got ${name.length}`,
    );
  }
  if (!VOLUME_NAME_RE.test(name)) {
    throw new Error(
      `volume name must match ^[a-zA-Z0-9_-]+$ (letters, digits, '_' and '-'), got ${JSON.stringify(name)}`,
    );
  }
}

/**
 * Reject volume IDs that are unsafe to embed in a URL path.
 *
 * Defense-in-depth against path traversal: a malicious ``volumeId`` such as
 * ``../other`` must not be interpolated into the request URL. The accepted
 * character class covers both named volumes and auto-generated UUIDs.
 */
function validateVolumeId(volumeId: string): string {
  if (typeof volumeId !== "string" || !volumeId) {
    throw new Error(`volumeId must be a non-empty string, got ${JSON.stringify(volumeId)}`);
  }
  if (!VOLUME_NAME_RE.test(volumeId)) {
    throw new Error(
      `volumeId must match ^[a-zA-Z0-9_-]+$ (letters, digits, '_' and '-'), got ${JSON.stringify(volumeId)}`,
    );
  }
  return volumeId;
}

function resolveVolumeId(volume: Volume | VolumeInfo | string): string {
  if (volume instanceof Volume || volume instanceof VolumeInfo) {
    return volume.volumeId;
  }
  if (typeof volume === "string") {
    return volume;
  }
  throw new Error(
    "volumeMounts value must be a Volume, VolumeInfo, VolumeMount or volume-id string",
  );
}

/**
 * Validate a mount path before forwarding it to the backend: a clean absolute
 * POSIX path with no ``.``/``..`` segments. The backend validates as well;
 * this turns a malicious or typo'd path into a clean error at the call site.
 */
function validateMountPath(path: string): string {
  if (typeof path !== "string" || !path) {
    throw new Error(`volume mount path must be a non-empty string, got ${JSON.stringify(path)}`);
  }
  if (!path.startsWith("/")) {
    throw new Error(
      `volume mount path must be absolute (start with '/'), got ${JSON.stringify(path)}`,
    );
  }
  for (const segment of path.split("/")) {
    if (segment === "." || segment === "..") {
      throw new Error(
        `volume mount path must not contain '.' or '..' segments, got ${JSON.stringify(path)}`,
      );
    }
  }
  return path;
}

/**
 * Serialize the e2b-style ``{path: volume}`` mapping into the wire format
 * shared with the Python and Go SDKs: ``[{name, path, readOnly?}]`` with
 * ``readOnly`` omitted when false.
 */
export function serializeVolumeMounts(mounts: VolumeMountsArg): Record<string, unknown>[] {
  const serialized: Record<string, unknown>[] = [];
  for (const [path, value] of Object.entries(mounts)) {
    const volume = value instanceof VolumeMount ? value.volume : value;
    const readOnly = value instanceof VolumeMount ? value.readOnly : false;
    const item: Record<string, unknown> = {
      name: validateVolumeId(resolveVolumeId(volume)),
      path: validateMountPath(path),
    };
    if (readOnly) {
      item.readOnly = true;
    }
    serialized.push(item);
  }
  return serialized;
}

async function checkVolumeResponse(resp: {
  ok: boolean;
  status: number;
  text: () => Promise<string>;
}): Promise<void> {
  if (resp.ok) {
    return;
  }
  let msg = "";
  try {
    const raw = (await resp.text()).trim();
    try {
      const body = JSON.parse(raw) as Record<string, unknown>;
      msg =
        (typeof body.message === "string" && body.message) ||
        (typeof body.detail === "string" && body.detail) ||
        raw;
    } catch {
      msg = raw;
    }
  } catch {
    msg = "";
  }
  if (!msg) {
    msg = `HTTP ${resp.status}`;
  }
  const code = resp.status;
  if (code === 401 || code === 403) {
    throw new AuthenticationError(msg, code);
  }
  if (code === 404) {
    throw new VolumeNotFoundError(msg, code);
  }
  // CubeMaster refuses to delete a mounted volume with "volume <id> is in use
  // by <n> node(s); ..." relayed as HTTP 409. The duplicate-name 409 ("volume
  // already exists") intentionally stays a generic ApiError.
  if (code === 409 && msg.toLowerCase().includes("in use")) {
    throw new VolumeInUseError(msg, code);
  }
  throw new ApiError(msg, code);
}

/**
 * Class-level helper for CubeSandbox persistent-volume management.
 *
 * e2b-compatible: methods mirror the e2b SDK. ``create`` and ``connect``
 * return a live {@link Volume} instance; ``list`` and ``getInfo`` return
 * plain {@link VolumeInfo} descriptors.
 */
export class Volume extends VolumeInfo {
  private constructor(info: VolumeInfo) {
    super(info.volumeId, info.name, info.token);
  }

  /** Masks the token, same as {@link VolumeInfo.toString}. */
  override toString(): string {
    return super.toString().replace("VolumeInfo(", "Volume(");
  }

  override [INSPECT](): string {
    return this.toString();
  }

  /**
   * POST /volumes — create a new persistent volume.
   *
   * e2b-compatible by default: when ``driver`` is omitted, no driver is sent
   * and the backend falls back to its *first configured* volume plugin. Pass a
   * non-empty ``driver`` to pin a specific plugin (a CubeSandbox extension),
   * e.g. ``"cos"`` or ``"nfs"`` — it must match a ``volume_plugins[].name``
   * entry in the CubeMaster config.
   *
   * ``name`` is optional: it must match ``^[a-zA-Z0-9_-]+$`` and be at most
   * {@link MAX_VOLUME_NAME_LEN} characters; when omitted the server generates
   * a UUID used as both the volume name and volume ID.
   */
  static async create(
    options: { name?: string; driver?: string; config?: Config | ConfigOptions } = {},
  ): Promise<Volume> {
    const name = options.name ?? "";
    validateName(name);
    const cfg = resolveConfig(options.config);
    const payload: Record<string, unknown> = { name };
    if (options.driver) {
      payload.driver = options.driver;
    }
    const resp = await controlFetch(cfg, `${cfg.apiUrl}/volumes`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });
    await checkVolumeResponse(resp);
    return new Volume(VolumeInfo.fromDict((await resp.json()) as Record<string, any>));
  }

  /**
   * GET /volumes — list all volumes. Tokens are only surfaced on
   * create / get-single, so ``token`` is always empty in list results.
   */
  static async list(
    options: { config?: Config | ConfigOptions } = {},
  ): Promise<VolumeInfo[]> {
    const cfg = resolveConfig(options.config);
    const resp = await controlFetch(cfg, `${cfg.apiUrl}/volumes`);
    await checkVolumeResponse(resp);
    const data = ((await resp.json()) as Record<string, any>[]) || [];
    return data.map((d) => VolumeInfo.fromDict(d));
  }

  /** GET /volumes/:id — fetch a single volume, including its token. */
  static async getInfo(
    volumeId: string,
    options: { config?: Config | ConfigOptions } = {},
  ): Promise<VolumeInfo> {
    validateVolumeId(volumeId);
    const cfg = resolveConfig(options.config);
    const resp = await controlFetch(cfg, `${cfg.apiUrl}/volumes/${volumeId}`);
    await checkVolumeResponse(resp);
    return VolumeInfo.fromDict((await resp.json()) as Record<string, any>);
  }

  /**
   * Connect to an existing volume (e2b compatible; wraps {@link getInfo}) and
   * return a live {@link Volume} instance.
   */
  static async connect(
    volumeId: string,
    options: { config?: Config | ConfigOptions } = {},
  ): Promise<Volume> {
    return new Volume(await Volume.getInfo(volumeId, options));
  }

  /**
   * DELETE /volumes/:id — permanently delete a volume.
   *
   * e2b-compatible: resolves ``true`` on success and ``false`` when the volume
   * does not exist (treated as idempotent — "already gone"). Deleting a volume
   * does **not** auto-detach it from running sandboxes: while any sandbox
   * still mounts it, the server refuses with a {@link VolumeInUseError}.
   */
  static async destroy(
    volumeId: string,
    options: { config?: Config | ConfigOptions } = {},
  ): Promise<boolean> {
    validateVolumeId(volumeId);
    const cfg = resolveConfig(options.config);
    const resp = await controlFetch(cfg, `${cfg.apiUrl}/volumes/${volumeId}`, {
      method: "DELETE",
    });
    if (resp.status === 404) {
      return false;
    }
    await checkVolumeResponse(resp);
    return true;
  }
}
