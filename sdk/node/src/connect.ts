// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Diagnostics for misrouted Connect stream requests, shared by commands,
// filesystem watch and PTY. Mirrors the Go SDK (sdk/go/connect.go and
// sdk/go/errors.go).

// Hints shared with the Go SDK. The routing hint is for HTML pages; a body
// that is not Connect-framed can also be a compressed response, so its hint
// names that cause too.
export const ROUTING_HINT =
  "check whether CUBE_PROXY_NODE_IP is configured or if the data-plane endpoint points to a web service";
export const NOT_AN_ENVELOPE_HINT =
  "check whether CUBE_PROXY_NODE_IP is configured, the data-plane endpoint points to a web service, or the response was compressed";

// Bounds for reading an HTTP error body, as in the Go SDK
// (maxRoutingPagePeekBytes, maxDiagnosticMsgLen, maxErrorBodyBytes).
const MAX_ROUTING_PAGE_PEEK_BYTES = 4096;
const MAX_DIAGNOSTIC_BYTES = 200;
const MAX_ERROR_BODY_BYTES = 64 * 1024;
// Upper bound on the time spent reading an error body, on top of any idle
// timeout of the caller: files.watchDir has none.
const ERROR_BODY_READ_TIMEOUT_MS = 10_000;

export interface ConnectResponse {
  status: number;
  headers: { get(name: string): string | null };
  body: ReadableStream<Uint8Array> | null;
}

function mediaType(resp: ConnectResponse): string {
  return (resp.headers.get("content-type") ?? "").split(";", 1)[0].trim().toLowerCase();
}

/**
 * Diagnose an HTTP 200 ``text/html`` response on a Connect stream. A Connect
 * stream is never labelled ``text/html``; such a response means the request
 * reached a Web UI or portal page instead of CubeProxy. Returns ``null`` for
 * anything else and never reads the body.
 */
export function htmlResponseError(resp: ConnectResponse): string | null {
  if (resp.status !== 200 || mediaType(resp) !== "text/html") {
    return null;
  }
  const contentType = resp.headers.get("content-type") ?? "";
  return `received HTML instead of a Connect response (content-type ${JSON.stringify(contentType)}); ${ROUTING_HINT}`;
}

function parseJson(text: string): { ok: true; value: unknown } | { ok: false } {
  try {
    return { ok: true, value: JSON.parse(text) };
  } catch {
    return { ok: false };
  }
}

/** Go's ``looksLikeJSONErrorDocument``: a JSON object with a non-empty message/detail/error string. */
function looksLikeJsonErrorDocument(trimmed: string): boolean {
  if (!trimmed.startsWith("{")) {
    return false;
  }
  const parsed = parseJson(trimmed);
  if (!parsed.ok || typeof parsed.value !== "object" || parsed.value === null || Array.isArray(parsed.value)) {
    return false;
  }
  const doc = parsed.value as Record<string, unknown>;
  const fields = [doc.message, doc.detail, doc.error];
  // Go decodes these fields as strings; any other type fails the decode.
  if (fields.some((value) => value !== undefined && value !== null && typeof value !== "string")) {
    return false;
  }
  return fields.some((value) => typeof value === "string" && value.trim() !== "");
}

/**
 * Go's ``isRoutingPage``: an HTML media type or HTML-looking body, or a 404
 * with a non-empty ``text/*`` body that is not JSON (for example Go's ``404
 * page not found``). A JSON API error document keeps the server's message
 * even when labelled ``text/html``, and possibly truncated JSON is left to
 * the API error message.
 */
export function isRoutingPage(status: number, type: string, body: Buffer): boolean {
  const trimmed = body.toString("utf-8").trim();
  if (trimmed.startsWith("{") && !parseJson(trimmed).ok) {
    return false;
  }
  const head = trimmed.slice(0, "<!doctype".length).toLowerCase();
  if (type === "text/html" || head.startsWith("<!doctype") || head.startsWith("<html")) {
    return !looksLikeJsonErrorDocument(trimmed);
  }
  if (status !== 404 || !type.startsWith("text/") || trimmed === "") {
    return false;
  }
  return !trimmed.startsWith("{");
}

function routingPageHint(status: number): string {
  if (status === 401 || status === 403) {
    return "received an HTML/text page instead of a Connect stream; check credentials or whether a portal is intercepting the request";
  }
  if (status >= 500) {
    return "received an HTML error page instead of a Connect stream; the request may have reached CubeProxy but the upstream envd or sandbox node is unreachable";
  }
  return `response may be an HTML/text page instead of a Connect stream; ${ROUTING_HINT}`;
}

/** Decode ``raw`` as UTF-8, dropping an incomplete trailing character. */
function decodePrefix(raw: Buffer): string {
  return new TextDecoder("utf-8").decode(raw, { stream: true });
}

/** Cut ``message`` to 200 UTF-8 bytes on a character boundary, as Go does. */
export function truncateDiagnostic(message: string): string {
  const raw = Buffer.from(message, "utf-8");
  if (raw.length <= MAX_DIAGNOSTIC_BYTES) {
    return message;
  }
  return `${decodePrefix(raw.subarray(0, MAX_DIAGNOSTIC_BYTES))}...`;
}

/** Replace control characters (ANSI escapes included) in server text. */
function sanitize(text: string): string {
  // Normalise CRLF/CR first so gateway pages keep their line breaks, then
  // replace C0 (except \t and \n), DEL and C1 controls, which include the
  // 8-bit CSI (U+009B) that terminals treat like ESC [.
  return text
    .replace(/\r\n?/g, "\n")
    .replace(/[\x00-\x08\x0b-\x1f\x7f-\x9f]/g, "\uFFFD");
}

/**
 * Read at most ``MAX_ERROR_BODY_BYTES + 1`` bytes of an error body, giving up
 * after ``ERROR_BODY_READ_TIMEOUT_MS``. The rest of the body is cancelled so
 * the connection is released. Returns ``null`` if nothing could be read.
 */
async function readErrorBody(body: ReadableStream<Uint8Array>): Promise<Buffer | null> {
  const reader = body.getReader();
  const chunks: Buffer[] = [];
  let length = 0;
  let failed = false;
  const timer = setTimeout(() => {
    void reader.cancel().catch(() => undefined);
  }, ERROR_BODY_READ_TIMEOUT_MS);
  (timer as { unref?: () => void }).unref?.();
  try {
    while (length <= MAX_ERROR_BODY_BYTES) {
      const { done, value } = await reader.read();
      if (done) {
        break;
      }
      chunks.push(Buffer.from(value));
      length += value.length;
    }
  } catch {
    failed = true;
  } finally {
    clearTimeout(timer);
    await reader.cancel().catch(() => undefined);
    reader.releaseLock();
  }
  if (failed && length === 0) {
    return null;
  }
  return Buffer.concat(chunks).subarray(0, MAX_ERROR_BODY_BYTES + 1);
}

/**
 * Describe an HTTP error response to a Connect stream request, like the Go
 * SDK's ``validateConnectResponse``. At most 64 KiB of the body is read. A
 * routing page (a misrouted request landing on a Web UI, gateway or portal)
 * is reported from its first 4 KiB, cut to 200 bytes and followed by a
 * status-specific hint. Any other body longer than 64 KiB is cut and marked
 * `` [truncated]``. Returns ``""`` when the body is missing or unreadable, so
 * the caller reports the status alone.
 */
export async function streamHttpErrorDetail(resp: ConnectResponse): Promise<string> {
  if (!resp.body) {
    return "";
  }
  const raw = await readErrorBody(resp.body);
  if (!raw) {
    return "";
  }
  const peek = raw.subarray(0, MAX_ROUTING_PAGE_PEEK_BYTES);
  if (isRoutingPage(resp.status, mediaType(resp), peek)) {
    const text = (raw.length > peek.length ? decodePrefix(peek) : peek.toString("utf-8")).trim();
    const hint = routingPageHint(resp.status);
    return text ? `${sanitize(truncateDiagnostic(text))}; ${hint}` : hint;
  }
  if (raw.length > MAX_ERROR_BODY_BYTES) {
    return `${sanitize(decodePrefix(raw.subarray(0, MAX_ERROR_BODY_BYTES)).trim())} [truncated]`;
  }
  return sanitize(raw.toString("utf-8").trim());
}
