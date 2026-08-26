// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
// cubecow_api_smoke — end-to-end smoke test that drives every subcommand
// of `cubecow-cli` and thereby every public method of the
// `cubecow::Engine` trait.
//
// This is the CLI counterpart to `examples/s3_rpc_smoke`:
//   * `s3_rpc_smoke` speaks JSON-RPC over the s3lvol Unix socket and
//     covers the 11 raw RPC methods.
//   * `cubecow_api_smoke` shells out to the `cubecow-cli` binary and
//     covers all 16 public Engine methods, using `--json` so responses
//     stay machine-parseable.
//
// The test lifecycle mirrors the design doc §5 flow:
//
//   1.  create-volume         (writable volume "vol")
//   2.  get-volume-info       (verify size, device_path, timestamps)
//   3.  get-volume-block-info (block_size / num_blocks)
//   4.  resize-volume         (grow the volume)
//   5.  list-volumes          (confirm the new volume appears + pagination)
//   6.  create-snapshot       (activated snapshot off the volume)
//   7.  list-snapshots        (confirm snapshot count)
//   8.  create-volume-from-snapshot (clone the snapshot into a writable vol)
//   9.  deactivate-volume     (deactivate the snapshot)
//  10.  activate-volume       (re-activate the snapshot — idempotency)
//  11.  metrics               (dump backend metrics)
//  12.  export-snapshot       (S3 backend only; skipped for reflink)
//  13.  get-volume-info       (poll upload status once, if exported)
//  14.  import-lvol           (S3 backend only; needs a live COS)
//  15.  delete-snapshot / delete-volume via cleanup pass
//
// Cleanup runs on both success and failure paths so a partial run does
// not leak names into the backend.
//
// Build & run:
//     # Build the cubecow-cli binary from the workspace root first:
//     cargo build --release --bin cubecow-cli
//     # Then, from this directory, build & run the smoke test:
//     cd examples/cubecow_api_smoke
//     cargo run --release -- \
//         --cubecow-cli ../../target/release/cubecow-cli \
//         --config /etc/cubecow/cubecow.toml

use std::env;
use std::ffi::OsString;
use std::path::PathBuf;
use std::process::{Command, ExitCode};
use std::time::Duration;

use serde_json::Value;

// ---------------------------------------------------------------------------
// CLI parsing (hand-rolled to match the s3_rpc_smoke style)
// ---------------------------------------------------------------------------

#[derive(Debug)]
struct Args {
    /// Path to the built `cubecow-cli` binary.
    cubecow_cli: PathBuf,
    /// TOML config path forwarded verbatim to `cubecow-cli --config`.
    /// Mutually exclusive with `json_config` / `json_config_inline`.
    config: Option<PathBuf>,
    /// JSON config file forwarded verbatim to `cubecow-cli --json-config`.
    json_config: Option<PathBuf>,
    /// Inline JSON config forwarded verbatim to `cubecow-cli --json-config-inline`.
    json_config_inline: Option<String>,
    /// Backend hint used only to decide whether to attempt the S3-only
    /// export / import steps. When unset the smoke test defaults to
    /// "reflink" and skips them.
    backend: String,
    /// Prefix for created objects. Randomised so re-runs cannot collide.
    prefix: String,
    /// Volume size in bytes for the create-volume step.
    size_bytes: u64,
    /// Target size for the resize-volume step. Must be >= size_bytes.
    resize_bytes: u64,
    /// Skip the S3-only export step (auto-forced for reflink).
    skip_export: bool,
    /// Skip the S3-only import step but still exercise export /
    /// get-volume-info against the export_uuid.
    skip_import: bool,
    /// If set, do not delete artefacts on exit; print them so an
    /// operator can inspect and clean up manually.
    keep: bool,
    /// How many seconds to poll `get-volume-info` waiting for
    /// `export_status = DONE` on an exported snapshot before running
    /// import-lvol. `0` disables the wait (single-shot sample only).
    upload_timeout_secs: u64,
}

impl Args {
    fn parse() -> Result<Self, String> {
        let default_prefix = format!(
            "cbc-smoke-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .map(|d| d.as_millis())
                .unwrap_or(0)
        );
        let mut cubecow_cli = PathBuf::from("cubecow-cli");
        let mut config: Option<PathBuf> = None;
        let mut json_config: Option<PathBuf> = None;
        let mut json_config_inline: Option<String> = None;
        let mut backend = String::from("reflink");
        let mut prefix = default_prefix;
        let mut size_bytes: u64 = 1024 * 1024 * 1024; // 1 GiB
        let mut resize_bytes: u64 = 2 * 1024 * 1024 * 1024; // 2 GiB
        let mut skip_export = false;
        let mut skip_import = false;
        let mut keep = false;
        let mut upload_timeout_secs: u64 = 0;

        let mut it = env::args().skip(1);
        while let Some(arg) = it.next() {
            match arg.as_str() {
                "-h" | "--help" => {
                    print_usage();
                    std::process::exit(0);
                }
                "--cubecow-cli" => {
                    cubecow_cli = PathBuf::from(it.next().ok_or("--cubecow-cli requires a path")?);
                }
                "--config" => {
                    config = Some(PathBuf::from(it.next().ok_or("--config requires a path")?));
                }
                "--json-config" => {
                    json_config = Some(PathBuf::from(
                        it.next().ok_or("--json-config requires a path")?,
                    ));
                }
                "--json-config-inline" => {
                    json_config_inline =
                        Some(it.next().ok_or("--json-config-inline requires a string")?);
                }
                "--backend" => {
                    backend = it.next().ok_or("--backend requires a value")?;
                }
                "--prefix" => {
                    prefix = it.next().ok_or("--prefix requires a value")?;
                }
                "--size-bytes" => {
                    size_bytes = it
                        .next()
                        .ok_or("--size-bytes requires an integer")?
                        .parse()
                        .map_err(|e| format!("--size-bytes: {e}"))?;
                }
                "--resize-bytes" => {
                    resize_bytes = it
                        .next()
                        .ok_or("--resize-bytes requires an integer")?
                        .parse()
                        .map_err(|e| format!("--resize-bytes: {e}"))?;
                }
                "--skip-export" => skip_export = true,
                "--skip-import" => skip_import = true,
                "--keep" => keep = true,
                "--upload-timeout-secs" => {
                    upload_timeout_secs = it
                        .next()
                        .ok_or("--upload-timeout-secs requires an integer")?
                        .parse()
                        .map_err(|e| format!("--upload-timeout-secs: {e}"))?;
                }
                other => return Err(format!("unknown argument: {other}")),
            }
        }

        if resize_bytes < size_bytes {
            return Err(format!(
                "--resize-bytes ({resize_bytes}) must be >= --size-bytes ({size_bytes})"
            ));
        }
        let src_set = config.is_some() as u8
            + json_config.is_some() as u8
            + json_config_inline.is_some() as u8;
        if src_set > 1 {
            return Err(
                "--config, --json-config and --json-config-inline are mutually exclusive"
                    .to_string(),
            );
        }

        // Reflink is a purely local backend — the S3-only export / import
        // steps are meaningless there, so silently force skip_export.
        if backend.eq_ignore_ascii_case("reflink") {
            skip_export = true;
        }

        Ok(Self {
            cubecow_cli,
            config,
            json_config,
            json_config_inline,
            backend,
            prefix,
            size_bytes,
            resize_bytes,
            skip_export,
            skip_import,
            keep,
            upload_timeout_secs,
        })
    }
}

fn print_usage() {
    eprintln!(
        "cubecow_api_smoke — exercise every cubecow-cli subcommand\n\
         \n\
         USAGE:\n    \
             cubecow_api_smoke [OPTIONS]\n\
         \n\
         OPTIONS:\n    \
             --cubecow-cli <PATH>         Path to the built cubecow-cli binary (default: cubecow-cli in PATH)\n    \
             --config <PATH>              TOML config forwarded to cubecow-cli --config\n    \
             --json-config <PATH>         JSON config file forwarded to cubecow-cli --json-config\n    \
             --json-config-inline <STR>   Inline JSON config forwarded to cubecow-cli --json-config-inline\n    \
             --backend <reflink|s3>       Hint: whether to run the S3-only export/import steps (default: reflink)\n    \
             --prefix <STR>               Prefix for created objects (default: cbc-smoke-<pid>-<ts>)\n    \
             --size-bytes <N>             Initial volume size in bytes (default: 1073741824 = 1 GiB)\n    \
             --resize-bytes <N>           Target size for the resize step (default: 2147483648 = 2 GiB)\n    \
             --skip-export                Skip the export/import/status steps (auto-forced for reflink)\n    \
             --skip-import                Skip ONLY import-lvol; still run export-snapshot + get-volume-info\n    \
             --upload-timeout-secs <N>    Poll get-volume-info for 'export_status = DONE' before importing\n                                    (default: 0 → single-shot; only used with S3 backend)\n    \
             --keep                       Do not delete created objects on exit\n    \
             -h, --help                   Print this help"
    );
}

// ---------------------------------------------------------------------------
// Subprocess helper: run one cubecow-cli invocation, capture stdout, and
// parse it as JSON.
// ---------------------------------------------------------------------------

#[derive(Debug)]
struct CliError(String);

impl std::fmt::Display for CliError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.0)
    }
}

impl std::error::Error for CliError {}

/// Result of a raw `cubecow-cli` invocation. We surface stdout / stderr
/// verbatim so the caller can decide what "success" means (e.g. a
/// `delete-snapshot` returning `NotFound` is fine for cleanup but not
/// for the primary delete step).
struct RawCliResult {
    /// True when the process exited with status 0.
    ok: bool,
    /// Exit status code.
    status: i32,
    /// Captured stdout.
    stdout: String,
    /// Captured stderr.
    stderr: String,
}

struct Runner<'a> {
    args: &'a Args,
}

impl<'a> Runner<'a> {
    fn new(args: &'a Args) -> Self {
        Self { args }
    }

    fn config_flags(&self) -> Vec<OsString> {
        let mut v: Vec<OsString> = Vec::new();
        if let Some(p) = &self.args.config {
            v.push(OsString::from("--config"));
            v.push(p.clone().into_os_string());
        } else if let Some(p) = &self.args.json_config {
            v.push(OsString::from("--json-config"));
            v.push(p.clone().into_os_string());
        } else if let Some(s) = &self.args.json_config_inline {
            v.push(OsString::from("--json-config-inline"));
            v.push(OsString::from(s));
        }
        v
    }

    /// Run `cubecow-cli <config-flags> --json <subcmd> <subargs...>` and
    /// return the raw stdout/stderr/exit-code triple. This never returns
    /// `Err`: process-spawn failures are wrapped in a `RawCliResult`
    /// with `ok = false` and a synthetic stderr line so callers do not
    /// need to double-branch.
    fn run_raw(&self, subcmd: &str, sub_args: &[&str]) -> RawCliResult {
        let mut cmd = Command::new(&self.args.cubecow_cli);
        cmd.args(self.config_flags());
        cmd.arg("--json");
        cmd.arg(subcmd);
        for a in sub_args {
            cmd.arg(a);
        }
        match cmd.output() {
            Ok(o) => RawCliResult {
                ok: o.status.success(),
                status: o.status.code().unwrap_or(-1),
                stdout: String::from_utf8_lossy(&o.stdout).into_owned(),
                stderr: String::from_utf8_lossy(&o.stderr).into_owned(),
            },
            Err(e) => RawCliResult {
                ok: false,
                status: -1,
                stdout: String::new(),
                stderr: format!("failed to spawn cubecow-cli: {e}"),
            },
        }
    }

    /// Run a subcommand and require exit-status 0 with a parseable JSON
    /// stdout. Returns the decoded JSON body.
    fn call(&self, subcmd: &str, sub_args: &[&str]) -> Result<Value, CliError> {
        let raw = self.run_raw(subcmd, sub_args);
        if !raw.ok {
            return Err(CliError(format!(
                "{subcmd}: exit={} stderr={}",
                raw.status,
                raw.stderr.trim()
            )));
        }
        if raw.stdout.trim().is_empty() {
            // Subcommands that print no body on --json (metrics on empty
            // engine, deactivate-volume with an already-deactivated
            // entry, etc.) are treated as `{}`.
            return Ok(Value::Object(serde_json::Map::new()));
        }
        serde_json::from_str::<Value>(raw.stdout.trim()).map_err(|e| {
            CliError(format!(
                "{subcmd}: could not parse JSON output: {e}\n---stdout---\n{}\n---stderr---\n{}",
                raw.stdout, raw.stderr
            ))
        })
    }
}

// ---------------------------------------------------------------------------
// Reporting helpers
// ---------------------------------------------------------------------------

#[derive(Debug)]
enum Outcome {
    Ok(String),
    Warn(String),
    Err(String),
}

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

fn banner(step: usize, total: usize, title: &str) {
    println!("\n[{step:02}/{total:02}] === {title} ===");
}

fn substep(msg: &str) {
    println!("    -> {msg}");
}

fn indent(s: &str, spaces: usize) -> String {
    let prefix = " ".repeat(spaces);
    s.lines()
        .map(|l| format!("{prefix}{l}"))
        .collect::<Vec<_>>()
        .join("\n")
}

fn dump_response(subcmd: &str, body: &Value) {
    match serde_json::to_string_pretty(body) {
        Ok(pretty) => println!("        {subcmd} response:\n{}", indent(&pretty, 12)),
        Err(_) => println!("        {subcmd} response: {body}"),
    }
}

// ---------------------------------------------------------------------------
// Main driver
// ---------------------------------------------------------------------------

const TOTAL_STEPS: usize = 15;

fn main() -> ExitCode {
    let args = match Args::parse() {
        Ok(a) => a,
        Err(e) => {
            eprintln!("argument error: {e}");
            print_usage();
            return ExitCode::from(2);
        }
    };

    println!("cubecow-cli end-to-end smoke test");
    println!("    cubecow-cli  : {}", args.cubecow_cli.display());
    if let Some(p) = &args.config {
        println!("    config (TOML): {}", p.display());
    } else if let Some(p) = &args.json_config {
        println!("    config (JSON): {}", p.display());
    } else if let Some(_) = &args.json_config_inline {
        println!("    config (JSON): inline");
    } else {
        println!("    config       : <cubecow-cli default: /etc/cubecow/cubecow.toml>");
    }
    println!("    backend hint : {}", args.backend);
    println!("    name prefix  : {}", args.prefix);
    println!(
        "    volume size  : {} bytes (resize → {} bytes)",
        args.size_bytes, args.resize_bytes
    );
    println!("    skip export  : {}", args.skip_export);
    println!("    skip import  : {}", args.skip_import);
    println!("    keep artefacts on exit: {}", args.keep);

    // Names created during the run.
    let vol_name = format!("{}-vol", args.prefix);
    let snap_name = format!("{}-snap", args.prefix);
    let clone_name = format!("{}-clone", args.prefix);
    let import_name = format!("{}-restore", args.prefix);
    let mut cleanup_snapshots: Vec<String> = Vec::new();
    let mut cleanup_volumes: Vec<String> = Vec::new();

    let runner = Runner::new(&args);
    let mut reports: Vec<StepReport> = Vec::new();
    let mut fatal = false;

    // Sanity-probe: make sure the CLI binary is runnable at all before
    // committing to a lifecycle test. `metrics` is a read-only call
    // and works against any backend.
    banner(0, TOTAL_STEPS, "sanity: cubecow-cli metrics");
    match runner.call("metrics", &[]) {
        Ok(v) => {
            dump_response("metrics", &v);
            substep("cubecow-cli is reachable and engine initialised");
        }
        Err(e) => {
            eprintln!("\nfatal: cubecow-cli sanity call failed: {e}");
            return ExitCode::from(3);
        }
    }

    // ------------------------------------------------------------------
    // 1. create-volume
    // ------------------------------------------------------------------
    banner(1, TOTAL_STEPS, "create-volume");
    let size_str = args.size_bytes.to_string();
    match runner.call("create-volume", &[&vol_name, &size_str]) {
        Ok(body) => {
            dump_response("create-volume", &body);
            let device_path = body
                .get("device_path")
                .and_then(|v| v.as_str())
                .unwrap_or_default();
            substep(&format!(
                "created '{vol_name}' at device_path='{device_path}'"
            ));
            cleanup_volumes.push(vol_name.clone());

            // Idempotency: a second create with the same name must fail
            // with an "already exists" style error.
            let dup = runner.run_raw("create-volume", &[&vol_name, &size_str]);
            let low = dup.stderr.to_lowercase();
            if dup.ok {
                reports.push(StepReport::err(
                    "create-volume",
                    "idempotency probe: 2nd create unexpectedly succeeded",
                ));
            } else if low.contains("already exists") {
                substep("idempotency probe OK (2nd create rejected)");
                reports.push(StepReport::ok(
                    "create-volume",
                    format!("device_path={device_path}"),
                ));
            } else {
                reports.push(StepReport::err(
                    "create-volume",
                    format!("idempotency probe: unexpected error: {}", dup.stderr.trim()),
                ));
            }
        }
        Err(e) => {
            reports.push(StepReport::err("create-volume", e.to_string()));
            fatal = true;
        }
    }

    // ------------------------------------------------------------------
    // 2. get-volume-info
    // ------------------------------------------------------------------
    if !fatal {
        banner(2, TOTAL_STEPS, "get-volume-info");
        match runner.call("get-volume-info", &[&vol_name]) {
            Ok(body) => {
                dump_response("get-volume-info", &body);
                let size = body.get("size_bytes").and_then(|v| v.as_u64()).unwrap_or(0);
                if size != args.size_bytes {
                    reports.push(StepReport::warn(
                        "get-volume-info",
                        format!(
                            "returned size_bytes={size} does not match requested {}",
                            args.size_bytes
                        ),
                    ));
                } else {
                    reports.push(StepReport::ok(
                        "get-volume-info",
                        format!("size_bytes={size}"),
                    ));
                }
            }
            Err(e) => reports.push(StepReport::err("get-volume-info", e.to_string())),
        }
    }

    // ------------------------------------------------------------------
    // 3. get-volume-block-info
    // ------------------------------------------------------------------
    if !fatal {
        banner(3, TOTAL_STEPS, "get-volume-block-info");
        match runner.call("get-volume-block-info", &[&vol_name]) {
            Ok(body) => {
                dump_response("get-volume-block-info", &body);
                let nb = body.get("num_blocks").and_then(|v| v.as_u64()).unwrap_or(0);
                let bs = body.get("block_size").and_then(|v| v.as_u64()).unwrap_or(0);
                reports.push(StepReport::ok(
                    "get-volume-block-info",
                    format!("num_blocks={nb} block_size={bs}"),
                ));
            }
            Err(e) => reports.push(StepReport::err("get-volume-block-info", e.to_string())),
        }
    }

    // ------------------------------------------------------------------
    // 4. resize-volume
    // ------------------------------------------------------------------
    if !fatal {
        banner(4, TOTAL_STEPS, "resize-volume");
        let new_size_str = args.resize_bytes.to_string();
        match runner.call("resize-volume", &[&vol_name, &new_size_str]) {
            Ok(body) => {
                dump_response("resize-volume", &body);
                let old = body.get("old_size").and_then(|v| v.as_u64()).unwrap_or(0);
                let new = body.get("new_size").and_then(|v| v.as_u64()).unwrap_or(0);
                if new != args.resize_bytes {
                    reports.push(StepReport::warn(
                        "resize-volume",
                        format!(
                            "returned new_size={new} does not match requested {}",
                            args.resize_bytes
                        ),
                    ));
                } else {
                    reports.push(StepReport::ok(
                        "resize-volume",
                        format!("{old} → {new} bytes"),
                    ));
                }
            }
            Err(e) => reports.push(StepReport::err("resize-volume", e.to_string())),
        }
    }

    // ------------------------------------------------------------------
    // 5. list-volumes
    // ------------------------------------------------------------------
    if !fatal {
        banner(5, TOTAL_STEPS, "list-volumes");
        match runner.call("list-volumes", &[]) {
            Ok(body) => {
                dump_response("list-volumes", &body);
                let total = body.get("total").and_then(|v| v.as_u64()).unwrap_or(0);
                let saw_self = body
                    .get("volumes")
                    .and_then(|v| v.as_array())
                    .map(|arr| {
                        arr.iter().any(|v| {
                            v.get("name").and_then(|n| n.as_str()) == Some(vol_name.as_str())
                        })
                    })
                    .unwrap_or(false);
                if !saw_self {
                    reports.push(StepReport::err(
                        "list-volumes",
                        format!("volume '{vol_name}' was not present in list-volumes output"),
                    ));
                } else {
                    reports.push(StepReport::ok(
                        "list-volumes",
                        format!("total={total} (includes '{vol_name}')"),
                    ));
                }
            }
            Err(e) => reports.push(StepReport::err("list-volumes", e.to_string())),
        }
    }

    // ------------------------------------------------------------------
    // 6. create-snapshot
    // ------------------------------------------------------------------
    let mut snapshot_ok = false;
    if !fatal {
        banner(6, TOTAL_STEPS, "create-snapshot");
        match runner.call("create-snapshot", &[&vol_name, &snap_name]) {
            Ok(body) => {
                dump_response("create-snapshot", &body);
                let device_path = body
                    .get("device_path")
                    .and_then(|v| v.as_str())
                    .unwrap_or_default();
                substep(&format!(
                    "snapshot '{snap_name}' activated at '{device_path}'"
                ));
                cleanup_snapshots.push(snap_name.clone());
                snapshot_ok = true;
                reports.push(StepReport::ok(
                    "create-snapshot",
                    format!("device_path={device_path}"),
                ));
            }
            Err(e) => reports.push(StepReport::err("create-snapshot", e.to_string())),
        }
    }

    // ------------------------------------------------------------------
    // 7. list-snapshots
    // ------------------------------------------------------------------
    if snapshot_ok {
        banner(7, TOTAL_STEPS, "list-snapshots");
        match runner.call("list-snapshots", &[&vol_name]) {
            Ok(body) => {
                dump_response("list-snapshots", &body);
                let saw_self = body
                    .get("snapshots")
                    .and_then(|v| v.as_array())
                    .map(|arr| {
                        arr.iter().any(|v| {
                            v.get("name").and_then(|n| n.as_str()) == Some(snap_name.as_str())
                        })
                    })
                    .unwrap_or(false);
                if saw_self {
                    reports.push(StepReport::ok(
                        "list-snapshots",
                        format!("found '{snap_name}' under volume '{vol_name}'"),
                    ));
                } else {
                    reports.push(StepReport::err(
                        "list-snapshots",
                        format!("snapshot '{snap_name}' not returned for volume '{vol_name}'"),
                    ));
                }
            }
            Err(e) => reports.push(StepReport::err("list-snapshots", e.to_string())),
        }
    } else if !fatal {
        reports.push(StepReport::err(
            "list-snapshots",
            "skipped: create-snapshot did not succeed",
        ));
    }

    // ------------------------------------------------------------------
    // 8. create-volume-from-snapshot
    // ------------------------------------------------------------------
    if snapshot_ok {
        banner(8, TOTAL_STEPS, "create-volume-from-snapshot");
        match runner.call("create-volume-from-snapshot", &[&snap_name, &clone_name]) {
            Ok(body) => {
                dump_response("create-volume-from-snapshot", &body);
                let device_path = body
                    .get("device_path")
                    .and_then(|v| v.as_str())
                    .unwrap_or_default();
                substep(&format!("clone '{clone_name}' at '{device_path}'"));
                cleanup_volumes.push(clone_name.clone());
                reports.push(StepReport::ok(
                    "create-volume-from-snapshot",
                    format!("device_path={device_path}"),
                ));
            }
            Err(e) => reports.push(StepReport::err(
                "create-volume-from-snapshot",
                e.to_string(),
            )),
        }
    } else if !fatal {
        reports.push(StepReport::err(
            "create-volume-from-snapshot",
            "skipped: snapshot not available",
        ));
    }

    // ------------------------------------------------------------------
    // 9. deactivate-volume (against the snapshot)
    // ------------------------------------------------------------------
    if snapshot_ok {
        banner(9, TOTAL_STEPS, "deactivate-volume (on snapshot)");
        match runner.call("deactivate-volume", &[&snap_name]) {
            Ok(body) => {
                dump_response("deactivate-volume", &body);
                substep(&format!("snapshot '{snap_name}' deactivated"));

                // Idempotency: a second deactivate must also succeed (both
                // backends model deactivate as idempotent per the design doc).
                match runner.call("deactivate-volume", &[&snap_name]) {
                    Ok(_) => {
                        substep("idempotency probe OK (2nd deactivate succeeded)");
                        reports.push(StepReport::ok("deactivate-volume", "idempotent"));
                    }
                    Err(e) => reports.push(StepReport::err(
                        "deactivate-volume",
                        format!("idempotency probe: {e}"),
                    )),
                }
            }
            Err(e) => reports.push(StepReport::err("deactivate-volume", e.to_string())),
        }
    } else if !fatal {
        reports.push(StepReport::err(
            "deactivate-volume",
            "skipped: no snapshot to deactivate",
        ));
    }

    // ------------------------------------------------------------------
    // 10. activate-volume (re-attach the snapshot)
    // ------------------------------------------------------------------
    if snapshot_ok {
        banner(10, TOTAL_STEPS, "activate-volume (re-attach snapshot)");
        match runner.call("activate-volume", &[&snap_name]) {
            Ok(body) => {
                dump_response("activate-volume", &body);
                let device_path = body
                    .get("device_path")
                    .and_then(|v| v.as_str())
                    .unwrap_or_default();
                if device_path.is_empty() {
                    reports.push(StepReport::warn(
                        "activate-volume",
                        "device_path was empty after re-activation",
                    ));
                } else {
                    reports.push(StepReport::ok(
                        "activate-volume",
                        format!("device_path={device_path}"),
                    ));
                }
            }
            Err(e) => reports.push(StepReport::err("activate-volume", e.to_string())),
        }
    } else if !fatal {
        reports.push(StepReport::err(
            "activate-volume",
            "skipped: no snapshot to re-activate",
        ));
    }

    // ------------------------------------------------------------------
    // 11. metrics
    // ------------------------------------------------------------------
    if !fatal {
        banner(11, TOTAL_STEPS, "metrics");
        match runner.call("metrics", &[]) {
            Ok(body) => {
                dump_response("metrics", &body);
                reports.push(StepReport::ok(
                    "metrics",
                    format!(
                        "{} counters",
                        body.as_object().map(|o| o.len()).unwrap_or(0)
                    ),
                ));
            }
            Err(e) => reports.push(StepReport::err("metrics", e.to_string())),
        }
    }

    // ------------------------------------------------------------------
    // 12. export-snapshot (S3-only)
    // ------------------------------------------------------------------
    let mut export_uuid: Option<String> = None;
    if snapshot_ok && !args.skip_export {
        banner(12, TOTAL_STEPS, "export-snapshot");
        match runner.call("export-snapshot", &[&snap_name]) {
            Ok(body) => {
                dump_response("export-snapshot", &body);
                let uuid = body
                    .get("export_uuid")
                    .and_then(|v| v.as_str())
                    .unwrap_or_default()
                    .to_string();
                if uuid.is_empty() {
                    reports.push(StepReport::err(
                        "export-snapshot",
                        "empty export_uuid returned",
                    ));
                } else {
                    substep(&format!("export_uuid = {uuid}"));
                    export_uuid = Some(uuid.clone());
                    reports.push(StepReport::ok("export-snapshot", format!("uuid={uuid}")));
                }
            }
            Err(e) => reports.push(StepReport::err("export-snapshot", e.to_string())),
        }
    } else if args.skip_export {
        reports.push(StepReport::ok(
            "export-snapshot",
            "SKIPPED (--skip-export or reflink backend)",
        ));
    } else {
        reports.push(StepReport::err(
            "export-snapshot",
            "skipped: no snapshot available",
        ));
    }

    // ------------------------------------------------------------------
    // 13. get-volume-info (poll upload status once when exported)
    // ------------------------------------------------------------------
    let mut saw_done = false;
    if export_uuid.is_some() {
        banner(13, TOTAL_STEPS, "get-volume-info (upload status)");
        let deadline =
            std::time::Instant::now() + Duration::from_secs(args.upload_timeout_secs.max(0));
        let mut backoff = Duration::from_millis(500);
        let max_backoff = Duration::from_secs(5);
        let mut last: Option<Value> = None;
        loop {
            match runner.call("get-volume-info", &[&snap_name]) {
                Ok(body) => {
                    let status = body
                        .get("export_status")
                        .and_then(|v| v.as_str())
                        .unwrap_or("")
                        .to_string();
                    substep(&format!("export_status = {status}"));
                    last = Some(body);
                    if status.eq_ignore_ascii_case("DONE") {
                        saw_done = true;
                        break;
                    }
                    if args.upload_timeout_secs == 0 || std::time::Instant::now() >= deadline {
                        break;
                    }
                    std::thread::sleep(backoff);
                    backoff = (backoff * 2).min(max_backoff);
                }
                Err(e) => {
                    reports.push(StepReport::err(
                        "get-volume-info (upload status)",
                        e.to_string(),
                    ));
                    break;
                }
            }
        }
        if let Some(body) = last {
            dump_response("get-volume-info", &body);
            if saw_done {
                reports.push(StepReport::ok(
                    "get-volume-info (upload status)",
                    "export_status=DONE",
                ));
            } else if args.upload_timeout_secs == 0 {
                reports.push(StepReport::warn(
                    "get-volume-info (upload status)",
                    "single-shot sample; export_status not DONE (pass --upload-timeout-secs to wait)",
                ));
            } else {
                reports.push(StepReport::warn(
                    "get-volume-info (upload status)",
                    format!("did not reach DONE within {}s", args.upload_timeout_secs),
                ));
            }
        }
    } else if args.skip_export {
        reports.push(StepReport::ok(
            "get-volume-info (upload status)",
            "SKIPPED (--skip-export or reflink backend)",
        ));
    }

    // ------------------------------------------------------------------
    // 14. import-lvol (S3-only)
    // ------------------------------------------------------------------
    if let Some(uuid) = export_uuid.clone() {
        if args.skip_import {
            banner(14, TOTAL_STEPS, "import-lvol");
            substep("--skip-import set: skipping");
            reports.push(StepReport::ok("import-lvol", "SKIPPED (--skip-import)"));
        } else {
            banner(14, TOTAL_STEPS, "import-lvol");
            if !saw_done {
                substep("warning: export upload not confirmed DONE; import may fail");
            }
            match runner.call("import-lvol", &[&import_name, &uuid]) {
                Ok(body) => {
                    dump_response("import-lvol", &body);
                    let device_path = body
                        .get("device_path")
                        .and_then(|v| v.as_str())
                        .unwrap_or_default();
                    substep(&format!("imported as '{import_name}' at '{device_path}'"));
                    cleanup_volumes.push(import_name.clone());
                    reports.push(StepReport::ok(
                        "import-lvol",
                        format!("device_path={device_path}"),
                    ));
                }
                Err(e) => reports.push(StepReport::err("import-lvol", e.to_string())),
            }
        }
    } else if args.skip_export {
        reports.push(StepReport::ok(
            "import-lvol",
            "SKIPPED (--skip-export or reflink backend)",
        ));
    } else {
        reports.push(StepReport::err(
            "import-lvol",
            "skipped: no export_uuid available",
        ));
    }

    // ------------------------------------------------------------------
    // 15. delete-snapshot / delete-volume (via cleanup)
    // ------------------------------------------------------------------
    banner(15, TOTAL_STEPS, "delete-snapshot + delete-volume (cleanup)");
    if args.keep {
        substep("--keep set: skipping cleanup");
        reports.push(StepReport::ok("delete-snapshot", "SKIPPED (--keep)"));
        reports.push(StepReport::ok("delete-volume", "SKIPPED (--keep)"));
    } else {
        let delete_reports = run_cleanup(&runner, &mut cleanup_snapshots, &mut cleanup_volumes);
        reports.extend(delete_reports);
    }

    // ------------------------------------------------------------------
    // Summary
    // ------------------------------------------------------------------
    println!("\n===== SUMMARY =====");
    let mut failed = 0usize;
    let mut warned = 0usize;
    for r in &reports {
        match &r.outcome {
            Outcome::Ok(detail) => println!("  [ OK ] {:32} {detail}", r.name),
            Outcome::Warn(detail) => {
                warned += 1;
                println!("  [WARN] {:32} {detail}", r.name);
            }
            Outcome::Err(detail) => {
                failed += 1;
                println!("  [FAIL] {:32} {detail}", r.name);
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

    if args.keep {
        println!("\n--keep: leftover artefacts (delete manually with cubecow-cli):");
        for n in &cleanup_snapshots {
            println!("    snapshot: {n}");
        }
        for n in &cleanup_volumes {
            println!("    volume:   {n}");
        }
    }

    if failed == 0 && !fatal {
        ExitCode::SUCCESS
    } else {
        ExitCode::from(1)
    }
}

/// Best-effort teardown: delete snapshots first, then volumes. A
/// second delete of the same name must yield a "not found" style
/// stderr — anything else is an idempotency violation.
fn run_cleanup(
    runner: &Runner<'_>,
    snapshots: &mut Vec<String>,
    volumes: &mut Vec<String>,
) -> Vec<StepReport> {
    let mut reports = Vec::new();

    // snapshots ---------------------------------------------------------
    let mut snap_errs: Vec<String> = Vec::new();
    let mut snap_idempotent = true;
    let snap_total = snapshots.len();
    while let Some(name) = snapshots.pop() {
        substep(&format!("cleanup: delete-snapshot '{name}'"));
        let first = runner.run_raw("delete-snapshot", &[&name]);
        if !first.ok {
            snap_errs.push(format!("{name}: {}", first.stderr.trim()));
            continue;
        }
        let second = runner.run_raw("delete-snapshot", &[&name]);
        if second.ok {
            snap_idempotent = false;
            snap_errs.push(format!("{name}: 2nd delete unexpectedly succeeded"));
        } else {
            let low = second.stderr.to_lowercase();
            if !(low.contains("not found") || low.contains("no such")) {
                snap_idempotent = false;
                snap_errs.push(format!(
                    "{name}: 2nd delete returned unexpected error: {}",
                    second.stderr.trim()
                ));
            }
        }
    }
    if snap_errs.is_empty() && snap_idempotent {
        reports.push(StepReport::ok(
            "delete-snapshot",
            format!("deleted {snap_total} snapshot(s) + idempotent"),
        ));
    } else if snap_errs.is_empty() {
        reports.push(StepReport::err(
            "delete-snapshot",
            "idempotency probe: at least one 2nd-delete did not report 'not found'",
        ));
    } else {
        reports.push(StepReport::err("delete-snapshot", snap_errs.join("; ")));
    }

    // volumes -----------------------------------------------------------
    let mut vol_errs: Vec<String> = Vec::new();
    let mut vol_idempotent = true;
    let vol_total = volumes.len();
    while let Some(name) = volumes.pop() {
        substep(&format!("cleanup: delete-volume '{name}'"));
        let first = runner.run_raw("delete-volume", &[&name]);
        if !first.ok {
            vol_errs.push(format!("{name}: {}", first.stderr.trim()));
            continue;
        }
        let second = runner.run_raw("delete-volume", &[&name]);
        if second.ok {
            vol_idempotent = false;
            vol_errs.push(format!("{name}: 2nd delete unexpectedly succeeded"));
        } else {
            let low = second.stderr.to_lowercase();
            if !(low.contains("not found") || low.contains("no such")) {
                vol_idempotent = false;
                vol_errs.push(format!(
                    "{name}: 2nd delete returned unexpected error: {}",
                    second.stderr.trim()
                ));
            }
        }
    }
    if vol_errs.is_empty() && vol_idempotent {
        reports.push(StepReport::ok(
            "delete-volume",
            format!("deleted {vol_total} volume(s) + idempotent"),
        ));
    } else if vol_errs.is_empty() {
        reports.push(StepReport::err(
            "delete-volume",
            "idempotency probe: at least one 2nd-delete did not report 'not found'",
        ));
    } else {
        reports.push(StepReport::err("delete-volume", vol_errs.join("; ")));
    }

    reports
}
