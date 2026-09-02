// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! cube-envd — CubeSandbox-maintained in-guest data-plane daemon.
//!
//! Speaks the E2B envd protocol (REST + ConnectRPC over JSON) on a single
//! port so existing SDKs and the CubeSandbox control plane keep working
//! unchanged. See README.md and issue #1227.
//!
//! "Upstream" / "baseline" throughout the code means the e2b Go envd
//! (`e2b-dev/infra`, pinned 0.5.13 / base image 2026.16) that this crate is
//! compatibility-tested against — see tests/e2e/envd_conformance.

mod auth;
mod cgroup;
mod connect;
mod cors;
mod error;
mod exec;
mod legacy;
mod msg;
mod rest;
mod server;
mod services;
mod state;

use std::sync::Arc;

const VERSION: &str = env!("CARGO_PKG_VERSION");
const COMMIT: &str = match option_env!("CUBE_ENVD_COMMIT") {
    Some(c) => c,
    None => "unknown",
};
const DEFAULT_PORT: u16 = 49983;

/// Flags upstream envd defines that cube-envd does not implement yet. They are
/// recognized — so a trailing bare value is never mistaken for a positional
/// argument — then warned about and ignored.
///
/// -cgroup-root is a deliberate non-feature (plan §6, item 1.8): upstream's
/// root override only matters when the daemon runs nested under another
/// cgroup root; add it if a nested conformance comparison ever needs it.
/// TODO: implement -cmd (command to run on daemon start).
const UNIMPLEMENTED: &[&str] = &["cmd", "cgroup-root"];

struct Cli {
    port: u16,
}

/// Exit status carried by `parse_cli`'s `Err` variant — a process exit code,
/// not an error value: `Err(0)` = the requested output (`-version` / `-commit`
/// / `-h`) was already printed, exit cleanly (Go's ErrHelp path); `Err(2)` =
/// usage error (Go `flag`'s ExitOnError code).
type ExitCode = i32;

/// Go-style flag parsing, mirroring upstream envd's `flag` package: one or two
/// leading dashes are both accepted, and the flags upstream rejects
/// (undefined, malformed, missing or invalid values) are rejected here too, so
/// a typo in `ENVD_EXTRA_ARGS` fails loudly instead of silently running on
/// defaults. Positional arguments are rejected as well — Go would stop parsing
/// and ignore them, which is the silent degradation this parser refuses.
fn parse_cli(args: &[String]) -> Result<Cli, ExitCode> {
    let mut port = DEFAULT_PORT;
    let mut version = false;
    let mut commit = false;
    let mut rest = args;

    while let Some(arg) = rest.first() {
        rest = &rest[1..];

        // Go: "--" terminates the flags (flag.go:1086) and leaves the rest as
        // positionals. envd has none, and dropping them silently is the same
        // degradation a bare positional causes, so reject them.
        if arg == "--" {
            if !rest.is_empty() {
                return Err(fail(&format!("unexpected arguments after --: {rest:?}")));
            }
            break;
        }
        // Go treats a bare "-" and non-flag tokens as positionals and stops
        // parsing (flag.go:1080). cube-envd has no positional arguments, so it
        // rejects them instead of silently running on defaults.
        if !arg.starts_with('-') || arg == "-" {
            return Err(fail(&format!("unexpected argument {arg:?}")));
        }
        let Some((name, inline)) = split_flag(arg) else {
            return Err(fail(&format!("bad flag syntax: {arg}")));
        };

        match name {
            // Go: ErrHelp exits 0 during the scan (flag.go:1168).
            "help" | "h" => {
                print_usage();
                return Err(0);
            }
            // Output actions are bare flags; attaching a value is a usage error.
            "version" | "commit" => {
                if inline.is_some() {
                    return Err(fail(&format!("flag -{name} does not take a value")));
                }
                if name == "version" {
                    version = true;
                } else {
                    commit = true;
                }
            }
            // cube-envd only implements the non-FC mode. Asking for FC
            // (-isnotfc=false) cannot be honored, so fail loudly instead of
            // silently pretending to run in FC mode.
            "isnotfc" => match inline {
                None | Some("1" | "t" | "T" | "true" | "TRUE" | "True") => {}
                Some("0" | "f" | "F" | "false" | "FALSE" | "False") => {
                    return Err(fail(
                        "-isnotfc=false is not supported: cube-envd only implements the non-FC mode",
                    ));
                }
                Some(value) => {
                    return Err(fail(&format!(
                        "invalid boolean value {value:?} for -isnotfc: parse error"
                    )));
                }
            },
            "port" => {
                let raw = match inline {
                    Some(v) => v,
                    None => {
                        let (v, tail) = rest
                            .split_first()
                            .ok_or_else(|| fail("flag needs an argument: -port"))?;
                        rest = tail;
                        v
                    }
                };
                port = raw.parse::<u16>().map_err(|_| {
                    fail(&format!(
                        "invalid value {raw:?} for flag -port: expected a port in 0-65535"
                    ))
                })?;
            }
            // Known but unimplemented: soak up a trailing bare value so it is
            // not taken for a positional argument. A '-'-prefixed token is
            // left for the next iteration, so a real flag after it still
            // parses (Go would swallow it and silently drop that flag).
            name if UNIMPLEMENTED.contains(&name) => {
                eprintln!("cube-envd: warning: -{name} is not implemented yet, ignoring it");
                if inline.is_none() {
                    if let Some((v, tail)) = rest.split_first() {
                        if !v.starts_with('-') {
                            rest = tail;
                        }
                    }
                }
            }
            _ => return Err(fail(&format!("flag provided but not defined: -{name}"))),
        }
    }

    // Go's main() reads versionFlag before commitFlag (main.go:135-145), so
    // both argv orders print the version.
    if let Some(text) = requested_output(version, commit) {
        println!("{text}");
        return Err(0);
    }
    Ok(Cli { port })
}

/// Version/commit adjudication. Extracted so a unit test can pin the order.
fn requested_output(version: bool, commit: bool) -> Option<&'static str> {
    if version {
        Some(VERSION)
    } else if commit {
        Some(COMMIT)
    } else {
        None
    }
}

/// Go strips one or two leading '-'; the remainder must be a non-empty name
/// starting with neither '-' nor '=' (flag.go:1084-1094), otherwise it is
/// reported as bad flag syntax.
fn split_flag(arg: &str) -> Option<(&str, Option<&str>)> {
    let body = arg.strip_prefix("--").or_else(|| arg.strip_prefix('-'))?;
    if body.is_empty() || body.starts_with('-') || body.starts_with('=') {
        return None;
    }
    Some(
        body.split_once('=')
            .map_or((body, None), |(n, v)| (n, Some(v))),
    )
}

/// Go's failf (flag.go:1056-1062): message first, then usage, both to stderr,
/// and ExitOnError turns it into exit code 2.
fn fail(msg: &str) -> ExitCode {
    eprintln!("{msg}");
    print_usage();
    2
}

fn print_usage() {
    eprintln!(
        "Usage of cube-envd:
  -port uint
        port on which the daemon should run (default {DEFAULT_PORT})
  -isnotfc
        accepted and ignored (compatibility with upstream envd); only the
        non-FC mode is implemented, so -isnotfc=false is rejected
  -cmd string
        NOT IMPLEMENTED: command to run on daemon start
  -cgroup-root string
        NOT IMPLEMENTED: cgroup root directory
  -version
        print the version
  -commit
        print the build commit"
    );
}

fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    let cli = match parse_cli(&args) {
        Ok(cli) => cli,
        Err(code) => std::process::exit(code),
    };

    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_env("ENVD_LOG_LEVEL")
                .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info")),
        )
        .with_target(false)
        .init();

    let runtime = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .expect("build tokio runtime");

    runtime.block_on(async move {
        let state = Arc::new(state::AppState::new().with_cgroup(cgroup::init()));
        let app = server::router(state);
        let addr = std::net::SocketAddr::from(([0, 0, 0, 0], cli.port));
        let listener = match tokio::net::TcpListener::bind(addr).await {
            Ok(l) => l,
            Err(e) => {
                tracing::error!("failed to bind {addr}: {e}");
                std::process::exit(1);
            }
        };
        tracing::info!("cube-envd {VERSION} ({COMMIT}) listening on {addr}");
        if let Err(e) = axum::serve(listener, app).await {
            tracing::error!("server error: {e}");
            std::process::exit(1);
        }
    });
}

#[cfg(test)]
mod tests {
    use super::*;

    fn args(items: &[&str]) -> Vec<String> {
        items.iter().map(|s| s.to_string()).collect()
    }

    fn port(items: &[&str]) -> Result<u16, ExitCode> {
        parse_cli(&args(items)).map(|c| c.port)
    }

    fn exit_code(items: &[&str]) -> Option<ExitCode> {
        parse_cli(&args(items)).err()
    }

    #[test]
    fn cli_defaults_and_port() {
        assert_eq!(port(&[]), Ok(DEFAULT_PORT));
        assert_eq!(port(&["-port", "8080", "-isnotfc"]), Ok(8080));
        assert_eq!(port(&["--port", "9000"]), Ok(9000));
        assert_eq!(port(&["-port=8080"]), Ok(8080));
        assert_eq!(port(&["--port=8080"]), Ok(8080));
    }

    /// `-cgroup-root` / `-cmd` are upstream flags cube-envd does not implement
    /// yet: recognized, warned about and skipped, so a flag after them still
    /// takes effect. (The test name predates that distinction.)
    #[test]
    fn cli_unknown_flags_ignored() {
        assert_eq!(
            port(&["-cgroup-root", "/sys/fs/cgroup", "-port", "7000"]),
            Ok(7000)
        );
        assert_eq!(port(&["-cmd", "/bin/sh", "-port", "7000"]), Ok(7000));
        // A '-'-prefixed token is never swallowed as an unimplemented flag's
        // value: Go would eat it and silently drop the following flag.
        assert_eq!(port(&["-cgroup-root", "-port", "7000"]), Ok(7000));
        // Trailing position: split_first() yields None, which must not panic.
        assert_eq!(port(&["-cgroup-root"]), Ok(DEFAULT_PORT));
        assert_eq!(port(&["-cmd"]), Ok(DEFAULT_PORT));
        assert_eq!(port(&["-cmd", "-cgroup-root"]), Ok(DEFAULT_PORT));
    }

    /// Flag names are matched exactly, like Go's `flag` — no abbreviation and
    /// no prefix matching, so a truncated name is an undefined flag.
    #[test]
    fn cli_flag_names_are_exact() {
        assert_eq!(port(&["-p", "8080"]), Err(2));
        assert_eq!(port(&["--por=8080"]), Err(2));
        assert_eq!(port(&["-PORT", "8080"]), Err(2));
    }

    #[test]
    fn cli_output_priority() {
        // Go's main() reads versionFlag before commitFlag, so version wins.
        assert_eq!(requested_output(true, true), Some(VERSION));
        assert_eq!(requested_output(true, false), Some(VERSION));
        assert_eq!(requested_output(false, true), Some(COMMIT));
        assert_eq!(requested_output(false, false), None);
    }

    #[test]
    fn cli_help_printed() {
        assert_eq!(exit_code(&["-h"]), Some(0));
        assert_eq!(exit_code(&["--help"]), Some(0));
        // ErrHelp short-circuits during the scan, so help wins either way.
        assert_eq!(exit_code(&["-h", "-version"]), Some(0));
        assert_eq!(exit_code(&["-version", "-h"]), Some(0));
    }

    #[test]
    fn cli_version_and_commit_exit_zero() {
        assert_eq!(exit_code(&["-version"]), Some(0));
        assert_eq!(exit_code(&["-commit"]), Some(0));
    }

    #[test]
    fn cli_output_flags_take_no_value() {
        assert_eq!(exit_code(&["-version=false"]), Some(2));
        assert_eq!(exit_code(&["-commit=false"]), Some(2));
    }

    #[test]
    fn cli_isnotfc_true_accepted() {
        assert_eq!(port(&["-isnotfc"]), Ok(DEFAULT_PORT));
        assert_eq!(port(&["-isnotfc=true"]), Ok(DEFAULT_PORT));
        assert_eq!(port(&["-isnotfc=1", "-port", "7000"]), Ok(7000));
    }

    #[test]
    fn cli_isnotfc_false_rejected() {
        // Asking for FC mode cannot be honored — fail instead of pretending.
        assert_eq!(exit_code(&["-isnotfc=false"]), Some(2));
        assert_eq!(exit_code(&["-isnotfc=0"]), Some(2));
    }

    #[test]
    fn cli_isnotfc_bad_value_rejected() {
        assert_eq!(exit_code(&["-isnotfc=maybe"]), Some(2));
    }

    #[test]
    fn cli_undefined_flag_is_rejected() {
        // The regression this parser exists for: a typo must not silently
        // fall back to the default port.
        assert_eq!(exit_code(&["-potr", "7000"]), Some(2));
        assert_eq!(exit_code(&["--potr=7000"]), Some(2));
    }

    #[test]
    fn cli_bad_flag_syntax() {
        assert_eq!(exit_code(&["---port", "7000"]), Some(2));
        assert_eq!(exit_code(&["--=x"]), Some(2));
        assert_eq!(exit_code(&["-=x"]), Some(2));
    }

    #[test]
    fn cli_flag_needs_argument() {
        assert_eq!(exit_code(&["-port"]), Some(2));
        assert_eq!(exit_code(&["--port"]), Some(2));
    }

    #[test]
    fn cli_invalid_port_value() {
        assert_eq!(port(&["-port", "abc"]), Err(2));
        assert_eq!(port(&["-port="]), Err(2));
        assert_eq!(port(&["-port", ""]), Err(2));
        // u16 range is enforced here; Go's int64 accepts it and only fails at bind.
        assert_eq!(port(&["-port", "99999"]), Err(2));
        // Go swallows the next token unconditionally, so a negative number is a
        // bad value rather than either a missing value or a new flag.
        assert_eq!(port(&["-port", "-7000"]), Err(2));
        assert_eq!(port(&["-port", "-1"]), Err(2));
    }

    #[test]
    fn cli_repeated_flag_last_wins() {
        assert_eq!(port(&["-port", "7000", "-port", "8000"]), Ok(8000));
        assert_eq!(port(&["-port", "7000", "-port", "abc"]), Err(2));
    }

    #[test]
    fn cli_error_beats_output() {
        // Go fails inside Parse(), so main() never reads versionFlag.
        assert_eq!(exit_code(&["-version", "-port", "abc"]), Some(2));
        assert_eq!(exit_code(&["-port", "abc", "-h"]), Some(2));
    }

    #[test]
    fn cli_positional_rejected() {
        // Go stops parsing and ignores these; cube-envd refuses them.
        assert_eq!(exit_code(&["foo"]), Some(2));
        assert_eq!(exit_code(&["-"]), Some(2));
        assert_eq!(exit_code(&["-isnotfc", "true"]), Some(2));
    }

    #[test]
    fn cli_double_dash_terminates() {
        assert_eq!(port(&["--"]), Ok(DEFAULT_PORT));
        assert_eq!(port(&["-port", "7000", "--"]), Ok(7000));
        // Go leaves anything after "--" as positionals; rejecting them here
        // keeps it consistent with bare positional arguments.
        assert_eq!(port(&["--", "foo"]), Err(2));
        assert_eq!(port(&["-port", "7000", "--", "bar"]), Err(2));
    }

    #[test]
    fn cli_port_boundary_values() {
        assert_eq!(port(&["-port", "0"]), Ok(0));
        assert_eq!(port(&["-port", "65535"]), Ok(65535));
        assert_eq!(port(&["-port", "65536"]), Err(2));
    }

    #[test]
    fn cli_unimplemented_with_equals() {
        // An inline value must not make the flag consume the next token too.
        assert_eq!(port(&["-cmd=foo", "-port", "7000"]), Ok(7000));
        assert_eq!(
            port(&["-cgroup-root=/sys/fs/cgroup", "-port", "7000"]),
            Ok(7000)
        );
    }

    #[test]
    fn cli_help_short_circuits_even_with_later_errors() {
        // ErrHelp returns during the scan, so trailing garbage is never seen.
        assert_eq!(exit_code(&["-h", "garbage"]), Some(0));
        assert_eq!(exit_code(&["-h", "-potr", "1"]), Some(0));
    }
}
