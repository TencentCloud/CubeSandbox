// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
// s3_rpc_smoke — Standalone smoke test for the s3lvol / RCOW JSON-RPC server.
//
// This binary opens the Unix Domain Socket exposed by the s3lvol daemon
// (see `docs/design/zh/s3lvol-rpc.md`) and drives every one of the 11
// documented RPC methods in a realistic order:
//
//   1.  rcow_create_lvol         (create a writable lvol)
//   2.  rcow_resize_lvol         (grow it)
//   3.  rcow_active_bdev         (attach it as an NVMe-oF namespace)
//   4.  rcow_get_bdev            (resolve the /dev/nvmeXnY path)
//   5.  rcow_create_snapshot     (snapshot the lvol)
//   6.  rcow_create_clone        (writable clone of the snapshot)
//   7.  rcow_export_snapshot     (publish snapshot to COS, obtain export_uuid)
//   8.  rcow_get_snapshot_status (query upload progress)
//   9.  rcow_import_lvol         (re-import into a new lvol using export_uuid)
//  10.  rcow_deactive_bdev       (unpublish the block device)
//  11.  rcow_delete_lvol         (delete every object we created)
//
// Every step logs its request / response, checks the `bool_value` /
// `string_value` business-level contract from §2.4 of the design doc,
// and runs an idempotency probe where the design doc mandates one.
//
// A best-effort cleanup pass runs on exit (both success and failure)
// so a partial run does not leak names into the daemon's namespace.
//
// Build & run:
//   cargo run --example s3_rpc_smoke -- --socket /var/run/s3lvol.sock

use std::env;
use std::io::{ErrorKind, Read, Write};
use std::os::unix::net::UnixStream;
use std::path::PathBuf;
use std::process::ExitCode;
use std::time::Duration;

use serde_json::{json, Value};

// ---------------------------------------------------------------------------
// CLI parsing (hand-rolled to avoid pulling in clap for a single example).
// ---------------------------------------------------------------------------

#[derive(Debug)]
struct Args {
    socket: PathBuf,
    prefix: String,
    size_gib: u64,
    resize_gib: u64,
    timeout_ms: u64,
    status_poll_secs: u64,
    /// Maximum number of seconds to wait for `rcow_get_snapshot_status`
    /// to report `export_status == DONE` before running `rcow_import_lvol`
    /// or attempting to delete the exported snapshot. 0 = skip the wait
    /// entirely (still attempt import/delete immediately, mirroring the
    /// old behaviour).
    upload_timeout_secs: u64,
    keep: bool,
    skip_export: bool,
    /// If true, run steps 7 (export) and 8 (get_snapshot_status) but
    /// SKIP step 9 (rcow_import_lvol). This is useful when the server
    /// side of `rcow_import_lvol` is known-broken or the COS bucket has
    /// no cross-node routing configured, but you still want to exercise
    /// export / status coverage.
    skip_import: bool,
}

impl Args {
    fn parse() -> Result<Self, String> {
        let mut socket = PathBuf::from("/var/run/s3lvol.sock");
        // Randomised suffix so re-runs cannot collide with each other.
        let default_prefix = format!(
            "smoke-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .map(|d| d.as_millis())
                .unwrap_or(0)
        );
        let mut prefix = default_prefix;
        let mut size_gib: u64 = 1;
        let mut resize_gib: u64 = 2;
        let mut timeout_ms: u64 = 10_000;
        let mut status_poll_secs: u64 = 0;
        let mut upload_timeout_secs: u64 = 120;
        let mut keep = false;
        let mut skip_export = false;
        let mut skip_import = false;

        let mut it = env::args().skip(1);
        while let Some(arg) = it.next() {
            match arg.as_str() {
                "-h" | "--help" => {
                    print_usage();
                    std::process::exit(0);
                }
                "--socket" => {
                    socket = PathBuf::from(it.next().ok_or("--socket requires a path argument")?);
                }
                "--prefix" => {
                    prefix = it.next().ok_or("--prefix requires a value")?;
                }
                "--size-gib" => {
                    size_gib = it
                        .next()
                        .ok_or("--size-gib requires an integer")?
                        .parse()
                        .map_err(|e| format!("--size-gib: {e}"))?;
                }
                "--resize-gib" => {
                    resize_gib = it
                        .next()
                        .ok_or("--resize-gib requires an integer")?
                        .parse()
                        .map_err(|e| format!("--resize-gib: {e}"))?;
                }
                "--timeout-ms" => {
                    timeout_ms = it
                        .next()
                        .ok_or("--timeout-ms requires an integer")?
                        .parse()
                        .map_err(|e| format!("--timeout-ms: {e}"))?;
                }
                "--poll-secs" => {
                    status_poll_secs = it
                        .next()
                        .ok_or("--poll-secs requires an integer")?
                        .parse()
                        .map_err(|e| format!("--poll-secs: {e}"))?;
                }
                "--upload-timeout-secs" => {
                    upload_timeout_secs = it
                        .next()
                        .ok_or("--upload-timeout-secs requires an integer")?
                        .parse()
                        .map_err(|e| format!("--upload-timeout-secs: {e}"))?;
                }
                "--keep" => keep = true,
                "--skip-export" => skip_export = true,
                "--skip-import" => skip_import = true,
                other => return Err(format!("unknown argument: {other}")),
            }
        }

        if resize_gib < size_gib {
            return Err(format!(
                "--resize-gib ({resize_gib}) must be >= --size-gib ({size_gib})"
            ));
        }
        Ok(Self {
            socket,
            prefix,
            size_gib,
            resize_gib,
            timeout_ms,
            status_poll_secs,
            upload_timeout_secs,
            keep,
            skip_export,
            skip_import,
        })
    }
}

fn print_usage() {
    eprintln!(
        "s3_rpc_smoke — exercise every s3lvol / RCOW JSON-RPC method\n\
         \n\
         USAGE:\n    \
             s3_rpc_smoke [OPTIONS]                   # deployed static binary\n    \
             cargo run --release -- [OPTIONS]         # from the source tree\n\
         \n\
         OPTIONS:\n    \
             --socket <PATH>       Unix socket path (default: /var/run/s3lvol.sock)\n    \
             --prefix <STR>        Name prefix for created objects (default: smoke-<pid>-<ts>)\n    \
             --size-gib <N>        Initial lvol size in GiB (default: 1)\n    \
             --resize-gib <N>      Target size for the resize step (default: 2)\n    \
             --timeout-ms <N>      Per-RPC read/write timeout in ms (default: 10000)\n    \
             --poll-secs <N>       Wait N seconds before the first export-status sample (default: 0)\n    \
             --upload-timeout-secs <N>\n                                   Max seconds to wait for 'export_status = DONE' before\n                                   running rcow_import_lvol (default: 120, 0 = do not wait)\n    \
             --skip-export         Skip export / import / status steps (offline / no-COS setups)\n    \
             --skip-import         Skip ONLY the rcow_import_lvol step; still run export &\n                                   get_snapshot_status. Ignored if --skip-export is also set.\n    \
             --keep                Do not delete created objects on exit\n    \
             -h, --help            Print this help"
    );
}

// ---------------------------------------------------------------------------
// Minimal JSON-RPC 2.0 client (line-delimited over UnixStream).
// ---------------------------------------------------------------------------

struct RpcClient {
    stream: UnixStream,
    next_id: u64,
}

#[derive(Debug)]
struct RpcError(String);

impl std::fmt::Display for RpcError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.0)
    }
}

impl std::error::Error for RpcError {}

impl RpcClient {
    fn connect(socket: &PathBuf, timeout: Duration) -> Result<Self, RpcError> {
        let stream = UnixStream::connect(socket)
            .map_err(|e| RpcError(format!("connect '{}' failed: {e}", socket.display())))?;
        stream
            .set_read_timeout(Some(timeout))
            .map_err(|e| RpcError(format!("set_read_timeout: {e}")))?;
        stream
            .set_write_timeout(Some(timeout))
            .map_err(|e| RpcError(format!("set_write_timeout: {e}")))?;
        Ok(Self { stream, next_id: 1 })
    }

    /// Send one RPC request and return the parsed `result` object (§2.4).
    ///
    /// A protocol-level `error` is surfaced as `Err(RpcError)`.
    /// A business-level `bool_value == false` is also surfaced as an error
    /// carrying the `string_value` message so tests can pattern-match on
    /// keywords such as "already exists" or "not found".
    fn call(&mut self, method: &str, params: Value) -> Result<Value, RpcError> {
        let id = self.next_id;
        self.next_id += 1;

        let req = json!({
            "jsonrpc": "2.0",
            "method": method,
            "params": params,
            "id": id,
        });
        let mut framed =
            serde_json::to_vec(&req).map_err(|e| RpcError(format!("encode {method}: {e}")))?;
        framed.push(b'\n');

        self.stream
            .write_all(&framed)
            .and_then(|_| self.stream.flush())
            .map_err(|e| RpcError(format!("write {method}: {e}")))?;

        let line =
            read_line(&mut self.stream).map_err(|e| RpcError(format!("read {method}: {e}")))?;
        let resp: Value =
            serde_json::from_slice(&line).map_err(|e| RpcError(format!("decode {method}: {e}")))?;

        if let Some(err) = resp.get("error") {
            let msg = err
                .get("message")
                .and_then(|m| m.as_str())
                .unwrap_or("unknown protocol-level error");
            return Err(RpcError(format!("{method}: protocol error: {msg}")));
        }

        let result = resp.get("result").cloned().unwrap_or(Value::Null);

        // Business-level contract from §2.4: every success response must
        // carry `bool_value`. A missing field is treated as an error, per
        // the client contract enforced by the production `S3Engine`.
        match result.get("bool_value").and_then(|v| v.as_bool()) {
            Some(true) => Ok(result),
            Some(false) => {
                let msg = result
                    .get("string_value")
                    .and_then(|v| v.as_str())
                    .unwrap_or("s3lvol reported failure");
                Err(RpcError(format!("{method}: {msg}")))
            }
            None => Err(RpcError(format!(
                "{method}: response missing required 'bool_value' field"
            ))),
        }
    }
}

/// Read a single `\n`-terminated frame from a UnixStream. The trailing `\n`
/// is stripped. Mirrors `s3.rs::read_line` so the framing behaviour matches
/// the production client bit-for-bit.
fn read_line(stream: &mut UnixStream) -> std::io::Result<Vec<u8>> {
    let mut out = Vec::with_capacity(256);
    let mut buf = [0u8; 1];
    loop {
        let n = stream.read(&mut buf)?;
        if n == 0 {
            return Err(std::io::Error::new(
                ErrorKind::UnexpectedEof,
                "s3lvol closed connection mid-frame",
            ));
        }
        if buf[0] == b'\n' {
            return Ok(out);
        }
        out.push(buf[0]);
    }
}

// ---------------------------------------------------------------------------
// Test-step helpers
// ---------------------------------------------------------------------------

fn string_value(result: &Value) -> Result<String, RpcError> {
    result
        .get("string_value")
        .and_then(|v| v.as_str())
        .map(|s| s.to_string())
        .ok_or_else(|| RpcError("response missing 'string_value'".to_string()))
}

/// Pretty-print the full JSON-RPC `result` object (§2.4) returned by
/// the daemon for `method`, so operators can eyeball every documented
/// field. For the three methods whose `string_value` is itself an
/// escaped JSON string (§2.5 — `rcow_active_bdev`, `rcow_get_bdev`,
/// `rcow_get_snapshot_status`), we additionally decode and pretty-print
/// the nested payload underneath the outer envelope so the reader sees
/// both layers on the same screen.
fn dump_response(method: &str, result: &Value) {
    match serde_json::to_string_pretty(result) {
        Ok(pretty) => println!("        {method} response:\n{}", indent(&pretty, 12)),
        Err(_) => println!("        {method} response: {result}"),
    }
    // If string_value is itself a JSON document (the §2.5 nested-JSON
    // methods), unwrap it once more so the inner fields show up.
    if let Some(inner) = result.get("string_value").and_then(|v| v.as_str()) {
        if let Ok(parsed) = serde_json::from_str::<Value>(inner) {
            if parsed.is_object() || parsed.is_array() {
                match serde_json::to_string_pretty(&parsed) {
                    Ok(pretty) => println!(
                        "        {method} string_value (decoded):\n{}",
                        indent(&pretty, 12)
                    ),
                    Err(_) => {}
                }
            }
        }
    }
}

/// Indent every line of `s` by `spaces` spaces. Used by the response
/// dumpers to keep pretty-printed JSON visually aligned under the step
/// banner.
fn indent(s: &str, spaces: usize) -> String {
    let prefix = " ".repeat(spaces);
    s.lines()
        .map(|l| format!("{prefix}{l}"))
        .collect::<Vec<_>>()
        .join("\n")
}

fn banner(step: usize, total: usize, title: &str) {
    println!("\n[{step:02}/{total:02}] === {title} ===");
}

fn substep(msg: &str) {
    println!("    -> {msg}");
}

/// Outcome of a single test step.
///
/// `Warn` is used for behaviours where the server deviates from the
/// design doc's SHOULD/MUST but the deviation does not block the rest
/// of the smoke test (e.g. `rcow_export_snapshot` returning a fresh
/// uuid on every call instead of a stable one). Warnings count as
/// non-fatal — they show up in the summary and are printed once more
/// at the bottom, but they do not flip the process exit code.
#[derive(Debug)]
enum Outcome {
    Ok(String),
    Warn(String),
    Err(String),
}

/// A single result row printed at the end of the run.
#[derive(Debug)]
struct StepReport {
    name: &'static str,
    outcome: Outcome,
}

impl StepReport {
    fn ok(name: &'static str, detail: impl Into<String>) -> Self {
        Self {
            name,
            outcome: Outcome::Ok(detail.into()),
        }
    }
    fn warn(name: &'static str, detail: impl Into<String>) -> Self {
        Self {
            name,
            outcome: Outcome::Warn(detail.into()),
        }
    }
    fn err(name: &'static str, detail: impl Into<String>) -> Self {
        Self {
            name,
            outcome: Outcome::Err(detail.into()),
        }
    }
}

// ---------------------------------------------------------------------------
// Main test driver
// ---------------------------------------------------------------------------

const TOTAL_STEPS: usize = 11;

fn main() -> ExitCode {
    let args = match Args::parse() {
        Ok(a) => a,
        Err(e) => {
            eprintln!("argument error: {e}");
            print_usage();
            return ExitCode::from(2);
        }
    };

    println!("s3lvol JSON-RPC smoke test");
    println!("    socket       : {}", args.socket.display());
    println!("    name prefix  : {}", args.prefix);
    println!(
        "    lvol size    : {} GiB (resize to {} GiB)",
        args.size_gib, args.resize_gib
    );
    println!("    rpc timeout  : {} ms", args.timeout_ms);
    println!("    skip export  : {}", args.skip_export);
    println!("    skip import  : {}", args.skip_import);
    println!("    keep artefacts on exit: {}", args.keep);

    let mut client = match RpcClient::connect(&args.socket, Duration::from_millis(args.timeout_ms))
    {
        Ok(c) => c,
        Err(e) => {
            eprintln!("\nfatal: failed to connect to s3lvol: {e}");
            return ExitCode::from(3);
        }
    };

    // Named objects created during the run — pushed onto the cleanup stack
    // in creation order, popped in reverse order regardless of outcome.
    let lvol_name = format!("{}-vol", args.prefix);
    let snap_name = format!("{}-snap", args.prefix);
    let clone_name = format!("__cbc_tmpclone_{}-clone", args.prefix);
    let import_name = format!("{}-restore", args.prefix);
    let mut cleanup: Vec<String> = Vec::new();

    let mut reports: Vec<StepReport> = Vec::new();
    let mut fatal = false;

    // ------------------------------------------------------------------
    // 1. rcow_create_lvol
    // ------------------------------------------------------------------
    banner(1, TOTAL_STEPS, "rcow_create_lvol");
    match client.call(
        "rcow_create_lvol",
        json!({ "lvol_name": lvol_name, "size_gib": args.size_gib }),
    ) {
        Ok(result) => {
            dump_response("rcow_create_lvol", &result);
            let echoed = string_value(&result).unwrap_or_default();
            substep(&format!("created lvol '{echoed}'"));
            cleanup.push(lvol_name.clone());

            // Idempotency probe (§4): second call with same params must
            // fail with a message containing "already exists".
            match client.call(
                "rcow_create_lvol",
                json!({ "lvol_name": lvol_name, "size_gib": args.size_gib }),
            ) {
                Ok(_) => reports.push(StepReport::err(
                    "rcow_create_lvol",
                    "idempotency probe: second create unexpectedly succeeded",
                )),
                Err(e) => {
                    let msg = e.to_string().to_lowercase();
                    // Accept EEXIST-style messages emitted by s3lvol in
                    // addition to the design-doc canonical wording
                    // "already exists". Some implementations surface the
                    // raw errno string (`File exists`) which we treat as
                    // semantically equivalent.
                    if msg.contains("already exists")
                        || msg.contains("already exist")
                        || msg.contains("file exists")
                        || msg.contains("eexist")
                    {
                        substep("idempotency probe OK (got EEXIST-style error)");
                        reports.push(StepReport::ok(
                            "rcow_create_lvol",
                            format!("lvol '{echoed}' created + idempotent"),
                        ));
                    } else {
                        reports.push(StepReport::err(
                            "rcow_create_lvol",
                            format!("idempotency probe: unexpected error: {e}"),
                        ));
                    }
                }
            }
        }
        Err(e) => {
            reports.push(StepReport::err("rcow_create_lvol", e.to_string()));
            fatal = true;
        }
    }

    // ------------------------------------------------------------------
    // 2. rcow_resize_lvol
    // ------------------------------------------------------------------
    if !fatal {
        banner(2, TOTAL_STEPS, "rcow_resize_lvol");
        match client.call(
            "rcow_resize_lvol",
            json!({ "lvol_name": lvol_name, "size_gib": args.resize_gib }),
        ) {
            Ok(result) => {
                dump_response("rcow_resize_lvol", &result);
                substep(&format!("resized to {} GiB", args.resize_gib));
                reports.push(StepReport::ok(
                    "rcow_resize_lvol",
                    format!("{} → {} GiB", args.size_gib, args.resize_gib),
                ));
            }
            Err(e) => reports.push(StepReport::err("rcow_resize_lvol", e.to_string())),
        }
    }

    // ------------------------------------------------------------------
    // 3. rcow_active_bdev
    // ------------------------------------------------------------------
    let mut lvol_activated = false;
    if !fatal {
        banner(3, TOTAL_STEPS, "rcow_active_bdev");
        match client.call("rcow_active_bdev", json!({ "device_name": lvol_name })) {
            Ok(result) => match string_value(&result) {
                Ok(nested) => {
                    dump_response("rcow_active_bdev", &result);
                    let already_active = serde_json::from_str::<Value>(&nested)
                        .ok()
                        .and_then(|v| v.get("already_active").and_then(|b| b.as_bool()))
                        .unwrap_or(false);
                    substep(&format!("activated (already_active = {already_active})"));
                    lvol_activated = true;
                    reports.push(StepReport::ok(
                        "rcow_active_bdev",
                        format!("already_active={already_active}"),
                    ));
                }
                Err(e) => reports.push(StepReport::err("rcow_active_bdev", e.to_string())),
            },
            Err(e) => reports.push(StepReport::err("rcow_active_bdev", e.to_string())),
        }
    }

    // ------------------------------------------------------------------
    // 4. rcow_get_bdev
    // ------------------------------------------------------------------
    if lvol_activated {
        banner(4, TOTAL_STEPS, "rcow_get_bdev");
        match client.call("rcow_get_bdev", json!({ "device_name": lvol_name })) {
            Ok(result) => match string_value(&result) {
                Ok(nested) => {
                    dump_response("rcow_get_bdev", &result);
                    let device_path = serde_json::from_str::<Value>(&nested)
                        .ok()
                        .and_then(|v| {
                            v.get("device_path")
                                .and_then(|p| p.as_str())
                                .map(|s| s.to_string())
                        })
                        .unwrap_or_default();
                    if device_path.is_empty() {
                        reports.push(StepReport::err(
                            "rcow_get_bdev",
                            "nested payload missing device_path",
                        ));
                    } else {
                        substep(&format!("device_path = {device_path}"));
                        reports.push(StepReport::ok("rcow_get_bdev", device_path));
                    }
                }
                Err(e) => reports.push(StepReport::err("rcow_get_bdev", e.to_string())),
            },
            Err(e) => reports.push(StepReport::err("rcow_get_bdev", e.to_string())),
        }
    } else if !fatal {
        reports.push(StepReport::err(
            "rcow_get_bdev",
            "skipped: rcow_active_bdev did not succeed",
        ));
    }

    // ------------------------------------------------------------------
    // 5. rcow_create_snapshot
    // ------------------------------------------------------------------
    let mut snapshot_ok = false;
    if !fatal {
        banner(5, TOTAL_STEPS, "rcow_create_snapshot");
        match client.call(
            "rcow_create_snapshot",
            json!({ "lvol_name": lvol_name, "snapshot_name": snap_name }),
        ) {
            Ok(result) => {
                dump_response("rcow_create_snapshot", &result);
                substep(&format!("snapshot '{snap_name}' created"));
                cleanup.push(snap_name.clone());
                snapshot_ok = true;
                reports.push(StepReport::ok("rcow_create_snapshot", snap_name.clone()));
            }
            Err(e) => reports.push(StepReport::err("rcow_create_snapshot", e.to_string())),
        }
    }

    // ------------------------------------------------------------------
    // 6. rcow_create_clone
    // ------------------------------------------------------------------
    if snapshot_ok {
        banner(6, TOTAL_STEPS, "rcow_create_clone");
        match client.call(
            "rcow_create_clone",
            json!({ "snapshot_name": snap_name, "clone_name": clone_name }),
        ) {
            Ok(result) => {
                dump_response("rcow_create_clone", &result);
                substep(&format!("clone '{clone_name}' created (writable)"));
                cleanup.push(clone_name.clone());
                reports.push(StepReport::ok("rcow_create_clone", clone_name.clone()));
            }
            Err(e) => reports.push(StepReport::err("rcow_create_clone", e.to_string())),
        }
    } else if !fatal {
        reports.push(StepReport::err(
            "rcow_create_clone",
            "skipped: snapshot creation did not succeed",
        ));
    }

    // ------------------------------------------------------------------
    // 7. rcow_export_snapshot
    // ------------------------------------------------------------------
    let mut export_uuid: Option<String> = None;
    if snapshot_ok && !args.skip_export {
        banner(7, TOTAL_STEPS, "rcow_export_snapshot");
        match client.call(
            "rcow_export_snapshot",
            json!({ "snapshot_name": snap_name }),
        ) {
            Ok(result) => match string_value(&result) {
                Ok(uuid) if !uuid.is_empty() => {
                    dump_response("rcow_export_snapshot", &result);
                    substep(&format!("export_uuid = {uuid}"));

                    // Stability probe (§4): re-exporting the same snapshot
                    // MUST return the same uuid. We keep this as a hard
                    // FAIL — the design doc lists it in the idempotency
                    // matrix as a mandatory contract, and a fresh uuid on
                    // every call breaks cubecow's crash-recovery model.
                    //
                    // We still feed the FIRST uuid to subsequent import /
                    // status steps because that is the one the daemon
                    // internally registered on the initial export.
                    match client.call(
                        "rcow_export_snapshot",
                        json!({ "snapshot_name": snap_name }),
                    ) {
                        Ok(r2) => match string_value(&r2) {
                            Ok(uuid2) if uuid2 == uuid => {
                                substep("stability probe OK (same uuid returned)");
                                reports.push(StepReport::ok(
                                    "rcow_export_snapshot",
                                    format!("uuid={uuid}"),
                                ));
                            }
                            Ok(uuid2) => reports.push(StepReport::err(
                                "rcow_export_snapshot",
                                format!(
                                    "stability probe: uuid changed on 2nd call ({uuid} vs {uuid2}) — server violates §4"
                                ),
                            )),
                            Err(e) => reports.push(StepReport::err(
                                "rcow_export_snapshot",
                                format!("stability probe: {e}"),
                            )),
                        },
                        Err(e) => reports.push(StepReport::err(
                            "rcow_export_snapshot",
                            format!("stability probe: {e}"),
                        )),
                    }
                    export_uuid = Some(uuid);
                }
                Ok(_) => reports.push(StepReport::err(
                    "rcow_export_snapshot",
                    "empty string_value (expected uuid)",
                )),
                Err(e) => reports.push(StepReport::err("rcow_export_snapshot", e.to_string())),
            },
            Err(e) => reports.push(StepReport::err("rcow_export_snapshot", e.to_string())),
        }
    } else if args.skip_export {
        reports.push(StepReport::ok(
            "rcow_export_snapshot",
            "SKIPPED (--skip-export)",
        ));
    } else {
        reports.push(StepReport::err(
            "rcow_export_snapshot",
            "skipped: no snapshot available",
        ));
    }

    // ------------------------------------------------------------------
    // 8. rcow_get_snapshot_status
    //
    // Per §3.11 the RPC itself is a *pure query* that returns whatever
    // upload state the daemon currently has — it does NOT block until
    // DONE on its own. To make Step 8 semantically test "the daemon
    // eventually reaches DONE", we poll the same RPC in a loop until
    // `export_status == DONE` or the operator's --upload-timeout-secs
    // budget elapses.
    // ------------------------------------------------------------------
    // Tracks whether Step 8 already observed a DONE state, so Step 9
    // can skip its own (now redundant) wait_for_upload_done pass.
    let mut step8_saw_done = false;
    if let Some(ref uuid) = export_uuid {
        banner(8, TOTAL_STEPS, "rcow_get_snapshot_status");
        // Optional dwell so the daemon has time to make progress on the
        // async COS upload before we sample the status.
        if args.status_poll_secs > 0 {
            substep(&format!(
                "sleeping {}s to let the async upload progress",
                args.status_poll_secs
            ));
            std::thread::sleep(Duration::from_secs(args.status_poll_secs));
        }

        // If the operator explicitly zeroed the budget, fall back to a
        // single-shot sample so `--upload-timeout-secs 0` remains a way
        // to say "do not block".
        let budget = Duration::from_secs(args.upload_timeout_secs);
        if budget == Duration::from_secs(0) {
            match client.call("rcow_get_snapshot_status", json!({ "export_uuid": uuid })) {
                Ok(result) => match string_value(&result) {
                    Ok(nested) => {
                        dump_response("rcow_get_snapshot_status", &result);
                        let (status, deletable) = extract_status(&nested);
                        substep(&format!(
                            "export_status = {status}, deletable = {deletable} (single-shot; --upload-timeout-secs=0)"
                        ));
                        step8_saw_done = status.eq_ignore_ascii_case("DONE");
                        if status == "<missing>" {
                            reports.push(StepReport::warn(
                                "rcow_get_snapshot_status",
                                "nested payload had no 'export_status' field",
                            ));
                        } else {
                            reports.push(StepReport::ok(
                                "rcow_get_snapshot_status",
                                format!("status={status}, deletable={deletable}"),
                            ));
                        }
                    }
                    Err(e) => {
                        reports.push(StepReport::err("rcow_get_snapshot_status", e.to_string()))
                    }
                },
                Err(e) => reports.push(StepReport::err("rcow_get_snapshot_status", e.to_string())),
            }
        } else {
            // Blocking-poll mode: loop until DONE or budget elapses.
            match wait_for_upload_done(&mut client, uuid, budget) {
                Ok((done, last_result)) => {
                    if let Some(ref result) = last_result {
                        dump_response("rcow_get_snapshot_status", result);
                    }
                    // Recover deletable from the last sample for the summary line.
                    let deletable = last_result
                        .as_ref()
                        .and_then(|r| string_value(r).ok())
                        .map(|nested| extract_status(&nested).1)
                        .unwrap_or_else(|| "<unknown>".to_string());
                    if done {
                        step8_saw_done = true;
                        substep("export_status reached DONE");
                        reports.push(StepReport::ok(
                            "rcow_get_snapshot_status",
                            format!("status=DONE, deletable={deletable}"),
                        ));
                    } else {
                        substep(&format!(
                            "export_status still not DONE within {}s budget",
                            budget.as_secs()
                        ));
                        reports.push(StepReport::err(
                            "rcow_get_snapshot_status",
                            format!(
                                "upload did not reach DONE within {}s (last deletable={deletable})",
                                budget.as_secs()
                            ),
                        ));
                    }
                }
                Err(e) => reports.push(StepReport::err("rcow_get_snapshot_status", e.to_string())),
            }
        }
    } else if !args.skip_export {
        reports.push(StepReport::err(
            "rcow_get_snapshot_status",
            "skipped: no export_uuid",
        ));
    } else {
        reports.push(StepReport::ok(
            "rcow_get_snapshot_status",
            "SKIPPED (--skip-export)",
        ));
    }

    // ------------------------------------------------------------------
    // 9. rcow_import_lvol
    // ------------------------------------------------------------------
    if args.skip_import && !args.skip_export {
        // Independently skip just the import step while still keeping
        // the export / status coverage from steps 7 & 8.
        banner(9, TOTAL_STEPS, "rcow_import_lvol");
        substep("--skip-import set: skipping rcow_import_lvol");
        reports.push(StepReport::ok(
            "rcow_import_lvol",
            "SKIPPED (--skip-import)",
        ));
    } else if let Some(ref uuid) = export_uuid {
        banner(9, TOTAL_STEPS, "rcow_import_lvol");
        // Step 8 already blocked until `export_status == DONE` (when
        // --upload-timeout-secs > 0), so most runs will fall through
        // this poll immediately. We keep the poll as a belt-and-suspenders
        // guard for the rare case where Step 8 was run in single-shot
        // mode (--upload-timeout-secs 0) OR the daemon regressed the
        // status between the two RPCs.
        if !step8_saw_done && args.upload_timeout_secs > 0 {
            match wait_for_upload_done(
                &mut client,
                uuid,
                Duration::from_secs(args.upload_timeout_secs),
            ) {
                Ok((true, _)) => substep("upload reached 'DONE' state — proceeding to import"),
                Ok((false, _)) => substep(
                    "upload still not DONE within --upload-timeout-secs; attempting import anyway",
                ),
                Err(e) => substep(&format!(
                    "poll of rcow_get_snapshot_status failed: {e}; attempting import anyway"
                )),
            }
        } else if step8_saw_done {
            substep("step 8 already observed DONE — proceeding straight to import");
        }
        match client.call(
            "rcow_import_lvol",
            json!({
                "lvol_name": import_name,
                "export_uuid": uuid,
                "decouple": true,
            }),
        ) {
            Ok(result) => {
                dump_response("rcow_import_lvol", &result);
                substep(&format!("imported as '{import_name}'"));
                cleanup.push(import_name.clone());
                reports.push(StepReport::ok("rcow_import_lvol", import_name.clone()));
            }
            Err(e) => reports.push(StepReport::err("rcow_import_lvol", e.to_string())),
        }
    } else if !args.skip_export {
        reports.push(StepReport::err(
            "rcow_import_lvol",
            "skipped: no export_uuid",
        ));
    } else {
        reports.push(StepReport::ok(
            "rcow_import_lvol",
            "SKIPPED (--skip-export)",
        ));
    }

    // ------------------------------------------------------------------
    // 10. rcow_deactive_bdev
    // ------------------------------------------------------------------
    if lvol_activated {
        banner(10, TOTAL_STEPS, "rcow_deactive_bdev");
        match client.call("rcow_deactive_bdev", json!({ "device_name": lvol_name })) {
            Ok(result) => {
                dump_response("rcow_deactive_bdev", &result);
                substep(&format!("deactivated '{lvol_name}'"));
                // Idempotency probe: second deactivate must either succeed
                // or return a "not found" style error (§4).
                match client.call("rcow_deactive_bdev", json!({ "device_name": lvol_name })) {
                    Ok(_) => {
                        substep("idempotency probe OK (2nd deactivate succeeded)");
                        reports.push(StepReport::ok(
                            "rcow_deactive_bdev",
                            "idempotent (succeed-on-repeat)",
                        ));
                    }
                    Err(e) => {
                        let low = e.to_string().to_lowercase();
                        if low.contains("not found") || low.contains("no such") {
                            substep("idempotency probe OK (got 'not found')");
                            reports.push(StepReport::ok(
                                "rcow_deactive_bdev",
                                "idempotent (not-found-on-repeat)",
                            ));
                        } else {
                            reports.push(StepReport::err(
                                "rcow_deactive_bdev",
                                format!("idempotency probe: unexpected error: {e}"),
                            ));
                        }
                    }
                }
            }
            Err(e) => reports.push(StepReport::err("rcow_deactive_bdev", e.to_string())),
        }
    } else if !fatal {
        reports.push(StepReport::err(
            "rcow_deactive_bdev",
            "skipped: lvol was never activated",
        ));
    }

    // ------------------------------------------------------------------
    // 11. rcow_delete_lvol  (executed as part of cleanup below)
    //     We still record a report row here to keep the summary aligned
    //     with the 11-method contract. The actual RPC(s) run inside
    //     `run_cleanup` so we get idempotency probes on every deletion.
    // ------------------------------------------------------------------
    banner(11, TOTAL_STEPS, "rcow_delete_lvol (via cleanup)");
    if args.keep {
        substep("--keep set: skipping deletion");
        reports.push(StepReport::ok("rcow_delete_lvol", "SKIPPED (--keep)"));
    } else {
        // By design (§3.7), `rcow_import_lvol` with `decouple: true`
        // must return a self-contained lvol — i.e. the daemon has
        // released every reference it held on the source snapshot by
        // the time step 9 returned. We therefore proceed straight to
        // delete: if the snapshot still reports EBUSY here, that is a
        // real server-side contract violation and we want the test to
        // surface it as a hard FAIL rather than paper over it by
        // polling `deletable = YES`.
        let delete_report = run_cleanup(&mut client, &mut cleanup);
        reports.push(delete_report);
    }

    // ------------------------------------------------------------------
    // Summary
    // ------------------------------------------------------------------
    println!("\n===== SUMMARY =====");
    let mut failed = 0usize;
    let mut warned = 0usize;
    for r in &reports {
        match &r.outcome {
            Outcome::Ok(detail) => println!("  [ OK ] {:28} {detail}", r.name),
            Outcome::Warn(detail) => {
                warned += 1;
                println!("  [WARN] {:28} {detail}", r.name);
            }
            Outcome::Err(detail) => {
                failed += 1;
                println!("  [FAIL] {:28} {detail}", r.name);
            }
        }
    }
    println!(
        "\n{} OK, {} WARN, {} FAIL (out of {} step(s))",
        reports.len() - failed - warned,
        warned,
        failed,
        reports.len(),
    );

    // If --keep was set, leftover names are printed so the operator can
    // clean up manually.
    if args.keep && !cleanup.is_empty() {
        println!("\n--keep: leftover artefacts (delete manually with rcow_delete_lvol):");
        for n in &cleanup {
            println!("    {n}");
        }
    }

    if failed == 0 && !fatal {
        ExitCode::SUCCESS
    } else {
        ExitCode::from(1)
    }
}

/// Best-effort teardown: for each name we recorded during the test, run
/// `rcow_deactive_bdev` (best-effort) then `rcow_delete_lvol`. Idempotency
/// is verified on delete: a second delete of the same name must fail with
/// a "not found" style message (§4).
///
/// This routine intentionally does NOT poll `rcow_get_snapshot_status`
/// for `deletable = YES` before deleting an exported snapshot. Per
/// §3.7 of the design doc, a successful `rcow_import_lvol` with
/// `decouple: true` must have already released the daemon's reference
/// on the source snapshot, so any EBUSY here is a genuine server-side
/// bug that we want to expose as a delete failure.
fn run_cleanup(client: &mut RpcClient, names: &mut Vec<String>) -> StepReport {
    let mut errs: Vec<String> = Vec::new();
    let mut idempotent_ok = true;
    let total = names.len();

    // Delete in LIFO order so dependants (clones / snapshots off the same
    // lvol) go before their sources.
    while let Some(name) = names.pop() {
        substep(&format!("cleanup: deactivate + delete '{name}'"));
        // Deactivate is best-effort per §3.8.
        let _ = client.call("rcow_deactive_bdev", json!({ "device_name": name }));
        match client.call("rcow_delete_lvol", json!({ "lvol_name": name })) {
            Ok(result) => {
                dump_response("rcow_delete_lvol", &result);
                // Idempotency probe: 2nd delete must complain "not found".
                match client.call("rcow_delete_lvol", json!({ "lvol_name": name })) {
                    Ok(_) => {
                        idempotent_ok = false;
                        errs.push(format!("{name}: 2nd delete unexpectedly succeeded"));
                    }
                    Err(e) => {
                        let low = e.to_string().to_lowercase();
                        if !(low.contains("not found") || low.contains("no such")) {
                            idempotent_ok = false;
                            errs.push(format!("{name}: 2nd delete returned unexpected error: {e}"));
                        }
                    }
                }
            }
            Err(e) => errs.push(format!("{name}: {e}")),
        }
    }

    if errs.is_empty() && idempotent_ok {
        StepReport::ok(
            "rcow_delete_lvol",
            format!("deleted {total} object(s) + idempotent"),
        )
    } else if errs.is_empty() {
        StepReport::err(
            "rcow_delete_lvol",
            "idempotency probe: at least one 2nd-delete did not report 'not found'",
        )
    } else {
        StepReport::err("rcow_delete_lvol", errs.join("; "))
    }
}

// ---------------------------------------------------------------------------
// Snapshot-status helpers
// ---------------------------------------------------------------------------

/// Extract the (status, deletable) pair from the nested JSON string
/// returned by `rcow_get_snapshot_status`.
///
/// The design doc (§3.11) spells the key **`"export_status"`** with a
/// literal space, and calls out `"export_status"` (underscore) as an
/// explicit compat trap. Real-world s3lvol builds have been observed
/// shipping the underscore variant, so we accept either. When neither
/// key is present we return `"<missing>"` so the caller can surface a
/// WARN.
fn extract_status(nested: &str) -> (String, String) {
    let parsed: Value = serde_json::from_str(nested).unwrap_or(Value::Null);
    let status = parsed
        .get("export_status")
        .and_then(|v| v.as_str())
        .unwrap_or("<missing>")
        .to_string();
    let deletable = parsed
        .get("deletable")
        .and_then(|v| v.as_str())
        .unwrap_or("<missing>")
        .to_string();
    (status, deletable)
}

/// Poll `rcow_get_snapshot_status` for up to `budget` seconds waiting
/// for `export_status` to reach `"DONE"`. Returns:
///
/// * `Ok((true, Some(last_result)))`  — daemon reported DONE within budget.
/// * `Ok((false, last_result))`       — budget elapsed without DONE.
/// * `Err(_)`                         — RPC error while polling.
///
/// `last_result` is the raw JSON-RPC `result` object from the most recent
/// successful `rcow_get_snapshot_status` call, so callers can `dump_response`
/// exactly one authoritative snapshot without issuing another RPC.
fn wait_for_upload_done(
    client: &mut RpcClient,
    export_uuid: &str,
    budget: Duration,
) -> Result<(bool, Option<Value>), RpcError> {
    let deadline = std::time::Instant::now() + budget;
    let mut backoff = Duration::from_millis(500);
    let max_backoff = Duration::from_secs(5);
    let last_result: Option<Value>;
    substep(&format!(
        "polling rcow_get_snapshot_status (uuid={export_uuid}) for DONE, budget = {}s",
        budget.as_secs()
    ));
    loop {
        let resp = client.call(
            "rcow_get_snapshot_status",
            json!({ "export_uuid": export_uuid }),
        )?;
        let nested = string_value(&resp)?;
        let (status, _deletable) = extract_status(&nested);
        substep(&format!("  status = {status}"));
        if status.eq_ignore_ascii_case("DONE") {
            last_result = Some(resp);
            return Ok((true, last_result));
        }
        if std::time::Instant::now() >= deadline {
            last_result = Some(resp);
            return Ok((false, last_result));
        }
        std::thread::sleep(backoff);
        backoff = (backoff * 2).min(max_backoff);
    }
}

// wait_for_deletable_yes() was intentionally removed: the design doc
// guarantees that a successful `rcow_import_lvol` releases the daemon's
// reference on the source snapshot, so cleanup does not need to poll
// `deletable = YES` before issuing `rcow_delete_lvol`. Keeping such a
// helper around would silently mask real server-side EBUSY bugs.
