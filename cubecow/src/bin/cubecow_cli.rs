// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
// cubecow-cli — a thin command-line wrapper around the public
// `cubecow::Engine` trait.
//
// This binary is intentionally hand-rolled (no clap dependency) so it
// stays consistent with `examples/s3_rpc_smoke` — the whole point of
// the CLI is to be a small, deployable script surface, not another
// library.
//
// Every subcommand maps 1:1 onto an `Engine` method:
//
//     Engine method                       | Subcommand
//     ------------------------------------|--------------------------------
//     create_volume                       | create-volume
//     delete_volume                       | delete-volume
//     resize_volume                       | resize-volume
//     get_volume_info                     | get-volume-info
//     get_volume_block_info               | get-volume-block-info
//     list_volumes                        | list-volumes
//     create_snapshot_from_volume         | create-snapshot
//     delete_snapshot                     | delete-snapshot
//     create_volume_from_snapshot         | create-volume-from-snapshot
//     list_snapshots                      | list-snapshots
//     activate_volume                     | activate-volume
//     deactivate_volume                   | deactivate-volume
//     export_snapshot                     | export-snapshot
//     import_lvol                         | import-lvol
//     reset_node_storage                  | reset-node-storage
//     metrics                             | metrics
//
// The engine is constructed once per invocation according to
// `--config <TOML>` (default: `/etc/cubecow/cubecow.toml`) or the
// mutually-exclusive `--json-config <FILE>` / `--json-config-inline <STR>`
// entrypoints, matching the `AppConfig::load` / `AppConfig::from_json_str`
// contract exposed by the library.
//
// Output is human-readable by default; pass `--json` to get a
// machine-parseable JSON representation of the API return value.
// Exit codes:
//
//     0  — command succeeded
//     1  — command failed at the engine layer (Engine method returned Err)
//     2  — bad CLI arguments
//     3  — engine initialisation failed
//
// Build & run:
//     cargo build --release --bin cubecow-cli
//     ./target/release/cubecow-cli --help
//     ./target/release/cubecow-cli --config /etc/cubecow/cubecow.toml create-volume vol-a 1073741824

use std::collections::HashMap;
use std::env;
use std::path::PathBuf;
use std::process::ExitCode;

use cubecow::config::AppConfig;
use cubecow::{initialize, initialize_without_logging, Engine, Snapshot, Volume, VolumeBlockInfo};
use serde_json::{json, Value};

// ---------------------------------------------------------------------------
// Global-flag / subcommand parsing (hand-rolled, no clap)
// ---------------------------------------------------------------------------

/// Sources for building the engine's `AppConfig`.
enum ConfigSource {
    TomlPath(PathBuf),
    JsonPath(PathBuf),
    JsonInline(String),
}

/// Result of parsing the top-level argv: shared engine-init options
/// plus the subcommand-specific tail (the caller's Vec<String> starting
/// at the subcommand name).
struct GlobalArgs {
    config: ConfigSource,
    with_logging: bool,
    json_output: bool,
    tail: Vec<String>,
}

fn print_usage() {
    eprintln!(
        "cubecow-cli — wrap every cubecow Engine API as a subcommand\n\
         \n\
         USAGE:\n    \
             cubecow-cli [GLOBAL OPTIONS] <SUBCOMMAND> [SUBCOMMAND ARGS]\n\
         \n\
         GLOBAL OPTIONS:\n    \
             --config <PATH>              Path to a TOML AppConfig (default: /etc/cubecow/cubecow.toml)\n    \
             --json-config <PATH>         Path to a JSON AppConfig (mutually exclusive with --config)\n    \
             --json-config-inline <STR>   Inline JSON AppConfig string (mutually exclusive with --config)\n    \
             --with-logging               Also install cubecow's internal tracing subscriber\n    \
             --json                       Emit machine-parseable JSON on stdout instead of a human-readable\n                                    text summary. Errors always print a short 'ERROR: ...' line to stderr.\n    \
             -h, --help                   Print this help\n\
         \n\
         SUBCOMMANDS (one per Engine API method):\n    \
             create-volume                <NAME> <SIZE_BYTES>\n    \
             delete-volume                <NAME>\n    \
             resize-volume                <NAME> <NEW_SIZE_BYTES>\n    \
             get-volume-info              <NAME>\n    \
             get-volume-block-info        <NAME>\n    \
             list-volumes                 [--page-size N] [--page-token TOKEN]\n    \
             create-snapshot              <SOURCE> <SNAPSHOT_NAME> [--no-activate]\n    \
             delete-snapshot              <SNAPSHOT_NAME>\n    \
             create-volume-from-snapshot  <SOURCE_SNAPSHOT> <VOLUME_NAME>\n    \
             list-snapshots               <VOLUME_NAME> [--page-size N] [--page-token TOKEN]\n    \
             activate-volume              <NAME>\n    \
             deactivate-volume            <NAME>\n    \
             export-snapshot              <SNAPSHOT_NAME>\n    \
             import-lvol                  <LVOL_NAME> <EXPORT_UUID>\n    \
             reset-node-storage           (destructive; wipes every volume + snapshot managed by cubecow)\n    \
             metrics\n    \
             help                         Print this help\n"
    );
}

fn parse_global_args() -> Result<GlobalArgs, String> {
    let mut it = env::args().skip(1).peekable();
    let mut toml_path: Option<PathBuf> = None;
    let mut json_path: Option<PathBuf> = None;
    let mut json_inline: Option<String> = None;
    let mut with_logging = false;
    let mut json_output = false;
    let mut tail: Vec<String> = Vec::new();

    while let Some(arg) = it.next() {
        match arg.as_str() {
            "-h" | "--help" | "help" if tail.is_empty() => {
                print_usage();
                std::process::exit(0);
            }
            "--config" => {
                toml_path = Some(PathBuf::from(it.next().ok_or("--config requires a path")?));
            }
            "--json-config" => {
                json_path = Some(PathBuf::from(
                    it.next().ok_or("--json-config requires a path")?,
                ));
            }
            "--json-config-inline" => {
                json_inline = Some(it.next().ok_or("--json-config-inline requires a string")?);
            }
            "--with-logging" => with_logging = true,
            "--json" => json_output = true,
            other => {
                // First non-flag token: this is the subcommand. Push it
                // and drain the rest of argv verbatim so the subcommand
                // handler can parse its own flags.
                tail.push(other.to_string());
                while let Some(rest) = it.next() {
                    tail.push(rest);
                }
                break;
            }
        }
    }

    let sources_set =
        toml_path.is_some() as u8 + json_path.is_some() as u8 + json_inline.is_some() as u8;
    if sources_set > 1 {
        return Err(
            "--config, --json-config and --json-config-inline are mutually exclusive".to_string(),
        );
    }
    let config = if let Some(p) = toml_path {
        ConfigSource::TomlPath(p)
    } else if let Some(p) = json_path {
        ConfigSource::JsonPath(p)
    } else if let Some(s) = json_inline {
        ConfigSource::JsonInline(s)
    } else {
        // Default: TOML at /etc/cubecow/cubecow.toml. Only surface an
        // error if the subcommand actually needs the engine and the
        // default path is missing.
        ConfigSource::TomlPath(PathBuf::from("/etc/cubecow/cubecow.toml"))
    };

    if tail.is_empty() {
        return Err("missing subcommand (see --help)".to_string());
    }

    Ok(GlobalArgs {
        config,
        with_logging,
        json_output,
        tail,
    })
}

/// Load an `AppConfig` from the resolved `ConfigSource`.
fn load_config(src: &ConfigSource) -> Result<AppConfig, String> {
    match src {
        ConfigSource::TomlPath(p) => AppConfig::load(p)
            .map_err(|e| format!("failed to load TOML config '{}': {e}", p.display())),
        ConfigSource::JsonPath(p) => {
            let raw = std::fs::read_to_string(p)
                .map_err(|e| format!("read JSON config '{}': {e}", p.display()))?;
            AppConfig::from_json_str(&raw)
                .map_err(|e| format!("parse JSON config '{}': {e}", p.display()))
        }
        ConfigSource::JsonInline(s) => {
            AppConfig::from_json_str(s).map_err(|e| format!("parse inline JSON config: {e}"))
        }
    }
}

/// Construct a `Box<dyn Engine>` from the resolved config source.
fn make_engine(src: &ConfigSource, with_logging: bool) -> Result<Box<dyn Engine>, String> {
    let cfg = load_config(src)?;
    let result = if with_logging {
        initialize(cfg)
    } else {
        initialize_without_logging(cfg)
    };
    result.map_err(|e| format!("engine initialization failed: {e}"))
}

// ---------------------------------------------------------------------------
// Small argument helpers
// ---------------------------------------------------------------------------

fn arg_required(pos: usize, args: &[String], name: &str) -> Result<String, String> {
    args.get(pos)
        .cloned()
        .ok_or_else(|| format!("missing required argument '{name}' (see --help)"))
}

fn parse_u64(name: &str, s: &str) -> Result<u64, String> {
    s.parse::<u64>()
        .map_err(|e| format!("'{name}' must be an unsigned integer ({s}): {e}"))
}

/// Extract `--page-size N` / `--page-token TOK` from a subcommand's
/// leftover args. Returns `(page_size, page_token)` and errors on any
/// unrecognised flag.
fn parse_pagination(args: &[String]) -> Result<(usize, Option<String>), String> {
    let mut page_size: usize = 0;
    let mut page_token: Option<String> = None;
    let mut i = 0;
    while i < args.len() {
        match args[i].as_str() {
            "--page-size" => {
                let v = args.get(i + 1).ok_or("--page-size requires an integer")?;
                page_size = v
                    .parse::<usize>()
                    .map_err(|e| format!("--page-size: {e}"))?;
                i += 2;
            }
            "--page-token" => {
                page_token = Some(
                    args.get(i + 1)
                        .cloned()
                        .ok_or("--page-token requires a value")?,
                );
                i += 2;
            }
            other => return Err(format!("unknown flag for list operation: {other}")),
        }
    }
    Ok((page_size, page_token))
}

// ---------------------------------------------------------------------------
// JSON projections for --json output
// ---------------------------------------------------------------------------

fn volume_to_json(v: &Volume) -> Value {
    json!({
        "name":           v.name,
        "size_bytes":     v.size_bytes,
        "device_path":    v.device_path,
        "snapshot_count": v.snapshot_count,
        "created_at":     v.created_at,
        "export_uuid":    v.export_uuid,
        "export_status":  v.export_status,
        "deletable":      v.deletable,
    })
}

fn snapshot_to_json(s: &Snapshot) -> Value {
    json!({
        "name":          s.name,
        "size_bytes":    s.size_bytes,
        "device_path":   s.device_path,
        "origin_volume": s.origin_volume,
        "created_at":    s.created_at,
        "export_uuid":   s.export_uuid,
        "export_status": s.export_status,
        "deletable":     s.deletable,
    })
}

fn block_info_to_json(b: &VolumeBlockInfo) -> Value {
    json!({
        "num_blocks": b.num_blocks,
        "block_size": b.block_size,
    })
}

// ---------------------------------------------------------------------------
// Pretty printers (human-readable, non-JSON output)
// ---------------------------------------------------------------------------

fn print_volume(v: &Volume) {
    println!("Volume:");
    println!("    name:            {}", v.name);
    println!("    size_bytes:      {}", v.size_bytes);
    println!("    device_path:     {}", v.device_path);
    println!("    snapshot_count:  {}", v.snapshot_count);
    println!("    created_at:      {}", v.created_at);
    println!("    export_uuid:     {}", v.export_uuid);
    println!("    export_status:   {}", v.export_status);
    match v.deletable {
        Some(b) => println!("    deletable:       {b}"),
        None => println!("    deletable:       <n/a>"),
    }
}

fn print_snapshot(s: &Snapshot) {
    println!("Snapshot:");
    println!("    name:            {}", s.name);
    println!("    size_bytes:      {}", s.size_bytes);
    println!("    device_path:     {}", s.device_path);
    println!("    origin_volume:   {}", s.origin_volume);
    println!("    created_at:      {}", s.created_at);
    println!("    export_uuid:     {}", s.export_uuid);
    println!("    export_status:   {}", s.export_status);
    match s.deletable {
        Some(b) => println!("    deletable:       {b}"),
        None => println!("    deletable:       <n/a>"),
    }
}

// ---------------------------------------------------------------------------
// Subcommand dispatch
// ---------------------------------------------------------------------------

fn run_subcommand(
    engine: &dyn Engine,
    subcmd: &str,
    args: &[String],
    json_output: bool,
) -> Result<(), String> {
    match subcmd {
        // ---- volume lifecycle -----------------------------------------
        "create-volume" => {
            let name = arg_required(0, args, "NAME")?;
            let size = parse_u64("SIZE_BYTES", &arg_required(1, args, "SIZE_BYTES")?)?;
            let v = engine
                .create_volume(&name, size)
                .map_err(|e| e.to_string())?;
            if json_output {
                println!("{}", volume_to_json(&v));
            } else {
                print_volume(&v);
            }
        }

        "delete-volume" => {
            let name = arg_required(0, args, "NAME")?;
            engine.delete_volume(&name).map_err(|e| e.to_string())?;
            if json_output {
                println!("{}", json!({ "deleted": name }));
            } else {
                println!("volume '{name}' deleted");
            }
        }

        "resize-volume" => {
            let name = arg_required(0, args, "NAME")?;
            let new_size = parse_u64("NEW_SIZE_BYTES", &arg_required(1, args, "NEW_SIZE_BYTES")?)?;
            let (old, new) = engine
                .resize_volume(&name, new_size)
                .map_err(|e| e.to_string())?;
            if json_output {
                println!("{}", json!({ "old_size": old, "new_size": new }));
            } else {
                println!("volume '{name}' resized: {old} → {new} bytes");
            }
        }

        "get-volume-info" => {
            let name = arg_required(0, args, "NAME")?;
            let v = engine.get_volume_info(&name).map_err(|e| e.to_string())?;
            if json_output {
                println!("{}", volume_to_json(&v));
            } else {
                print_volume(&v);
            }
        }

        "get-volume-block-info" => {
            let name = arg_required(0, args, "NAME")?;
            let b = engine
                .get_volume_block_info(&name)
                .map_err(|e| e.to_string())?;
            if json_output {
                println!("{}", block_info_to_json(&b));
            } else {
                println!("VolumeBlockInfo:");
                println!("    num_blocks: {}", b.num_blocks);
                println!("    block_size: {}", b.block_size);
            }
        }

        "list-volumes" => {
            let (page_size, page_token) = parse_pagination(args)?;
            let (vols, next, total) = engine.list_volumes(page_size, page_token.as_deref());
            if json_output {
                let out = json!({
                    "total":            total,
                    "next_page_token":  next,
                    "volumes":          vols.iter().map(volume_to_json).collect::<Vec<_>>(),
                });
                println!("{out}");
            } else {
                println!("total volumes: {total}");
                if let Some(tok) = &next {
                    println!("next_page_token: {tok}");
                }
                for (i, v) in vols.iter().enumerate() {
                    println!(
                        "  [{i}] name={} size={} device={}",
                        v.name, v.size_bytes, v.device_path
                    );
                }
            }
        }

        // ---- snapshot lifecycle ---------------------------------------
        "create-snapshot" => {
            let source = arg_required(0, args, "SOURCE")?;
            let snap = arg_required(1, args, "SNAPSHOT_NAME")?;
            // Default is activate = true; --no-activate flips it off.
            let mut activate = true;
            for flag in &args[2..] {
                match flag.as_str() {
                    "--no-activate" => activate = false,
                    other => return Err(format!("unknown flag for create-snapshot: {other}")),
                }
            }
            let s = engine
                .create_snapshot_from_volume(&source, &snap, activate)
                .map_err(|e| e.to_string())?;
            if json_output {
                println!("{}", snapshot_to_json(&s));
            } else {
                print_snapshot(&s);
            }
        }

        "delete-snapshot" => {
            let name = arg_required(0, args, "SNAPSHOT_NAME")?;
            engine.delete_snapshot(&name).map_err(|e| e.to_string())?;
            if json_output {
                println!("{}", json!({ "deleted": name }));
            } else {
                println!("snapshot '{name}' deleted");
            }
        }

        "create-volume-from-snapshot" => {
            let src = arg_required(0, args, "SOURCE_SNAPSHOT")?;
            let vol = arg_required(1, args, "VOLUME_NAME")?;
            let v = engine
                .create_volume_from_snapshot(&src, &vol)
                .map_err(|e| e.to_string())?;
            if json_output {
                println!("{}", volume_to_json(&v));
            } else {
                print_volume(&v);
            }
        }

        "list-snapshots" => {
            let vol = arg_required(0, args, "VOLUME_NAME")?;
            let (page_size, page_token) = parse_pagination(&args[1..])?;
            let (snaps, next) = engine.list_snapshots(&vol, page_size, page_token.as_deref());
            if json_output {
                let out = json!({
                    "next_page_token": next,
                    "snapshots":       snaps.iter().map(snapshot_to_json).collect::<Vec<_>>(),
                });
                println!("{out}");
            } else {
                println!("snapshots of volume '{vol}': {}", snaps.len());
                if let Some(tok) = &next {
                    println!("next_page_token: {tok}");
                }
                for (i, s) in snaps.iter().enumerate() {
                    println!(
                        "  [{i}] name={} origin={} size={} device={}",
                        s.name, s.origin_volume, s.size_bytes, s.device_path
                    );
                }
            }
        }

        // ---- activation -----------------------------------------------
        "activate-volume" => {
            let name = arg_required(0, args, "NAME")?;
            let v = engine.activate_volume(&name).map_err(|e| e.to_string())?;
            if json_output {
                println!("{}", volume_to_json(&v));
            } else {
                print_volume(&v);
            }
        }

        "deactivate-volume" => {
            let name = arg_required(0, args, "NAME")?;
            engine.deactivate_volume(&name).map_err(|e| e.to_string())?;
            if json_output {
                println!("{}", json!({ "deactivated": name }));
            } else {
                println!("'{name}' deactivated");
            }
        }

        // ---- cross-node export / import (S3 backend only) --------------
        "export-snapshot" => {
            let name = arg_required(0, args, "SNAPSHOT_NAME")?;
            let uuid = engine.export_snapshot(&name).map_err(|e| e.to_string())?;
            if json_output {
                println!("{}", json!({ "export_uuid": uuid }));
            } else {
                println!("export_uuid: {uuid}");
            }
        }

        "import-lvol" => {
            let name = arg_required(0, args, "LVOL_NAME")?;
            let uuid = arg_required(1, args, "EXPORT_UUID")?;
            let v = engine
                .import_lvol(&name, &uuid)
                .map_err(|e| e.to_string())?;
            if json_output {
                println!("{}", volume_to_json(&v));
            } else {
                print_volume(&v);
            }
        }

        // ---- node ops --------------------------------------------------
        "reset-node-storage" => {
            engine.reset_node_storage().map_err(|e| e.to_string())?;
            if json_output {
                println!("{}", json!({ "reset": true }));
            } else {
                println!("node storage reset");
            }
        }

        "metrics" => {
            let m: HashMap<String, u64> = engine.metrics();
            if json_output {
                println!("{}", json!(m));
            } else {
                let mut kv: Vec<(&String, &u64)> = m.iter().collect();
                kv.sort_by(|a, b| a.0.cmp(b.0));
                for (k, v) in kv {
                    println!("{k} = {v}");
                }
            }
        }

        "help" | "-h" | "--help" => {
            print_usage();
        }

        other => {
            return Err(format!("unknown subcommand: {other}"));
        }
    }
    Ok(())
}

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

fn main() -> ExitCode {
    let globals = match parse_global_args() {
        Ok(g) => g,
        Err(e) => {
            eprintln!("ERROR: {e}");
            print_usage();
            return ExitCode::from(2);
        }
    };

    // Fast-path: `cubecow-cli help` / `cubecow-cli --help` should not
    // require a config file.
    let subcmd = globals.tail[0].clone();
    let sub_args: Vec<String> = globals.tail[1..].to_vec();
    if matches!(subcmd.as_str(), "help" | "-h" | "--help") {
        print_usage();
        return ExitCode::SUCCESS;
    }

    let engine = match make_engine(&globals.config, globals.with_logging) {
        Ok(e) => e,
        Err(e) => {
            eprintln!("ERROR: {e}");
            return ExitCode::from(3);
        }
    };

    match run_subcommand(engine.as_ref(), &subcmd, &sub_args, globals.json_output) {
        Ok(()) => ExitCode::SUCCESS,
        Err(e) => {
            eprintln!("ERROR: {e}");
            ExitCode::from(1)
        }
    }
}
