// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use clap::{Parser, Subcommand, ValueEnum};

#[derive(Debug, Clone, Copy, ValueEnum)]
pub enum LogFormat {
    Text,
    Json,
}

#[derive(Debug, Parser)]
#[command(
    name = "cube-envd",
    about = "CubeSandbox envd data-plane daemon",
    disable_version_flag = true
)]
pub struct Cli {
    #[command(subcommand)]
    pub command: Command,
}

#[derive(Debug, Subcommand)]
pub enum Command {
    /// Print E2B compatibility version (legacy `-version` / `--version`).
    #[command(name = "version", alias = "Version")]
    Version,
    /// Print the cube-envd product version.
    #[command(name = "cube-version")]
    CubeVersion,
    /// Print the full Cube source revision.
    #[command(name = "commit")]
    Commit,
    /// Run the envd HTTP server (default when no subcommand is given).
    #[command(name = "serve")]
    Serve(ServeConfig),
}

#[derive(Debug, Parser, Clone)]
#[command(args_override_self = true)]
pub struct ServeConfig {
    /// Listen port (legacy `-port` / `--port`, default 49983).
    #[arg(short = 'p', long = "port", default_value_t = 49983, alias = "Port", value_parser = parse_go_integer)]
    pub port: i64,
    /// Select non-Firecracker sandbox environment and marker values.
    #[arg(long = "isnotfc", alias = "isNotFc", action = clap::ArgAction::Set, num_args = 0..=1, require_equals = true, default_missing_value = "true", default_value = "false", value_parser = parse_go_bool)]
    pub is_not_fc: bool,
    #[arg(long = "cmd", default_value = "")]
    pub start_cmd: String,
    #[arg(
        long = "cgroup-root",
        default_value = "/sys/fs/cgroup",
        help = "cgroup root directory"
    )]
    pub cgroup_root: String,
    #[arg(long = "log-format", value_enum, default_value_t = LogFormat::Text)]
    pub log_format: LogFormat,
    #[arg(long = "version", action = clap::ArgAction::Set, num_args = 0..=1, require_equals = true, default_missing_value = "true", default_value = "false", value_parser = parse_go_bool)]
    version: bool,
    #[arg(long = "commit", action = clap::ArgAction::Set, num_args = 0..=1, require_equals = true, default_missing_value = "true", default_value = "false", value_parser = parse_go_bool)]
    commit: bool,
}

impl Cli {
    pub fn parse() -> Self {
        Self::try_parse_from_compat(std::env::args()).unwrap_or_else(|err| {
            if err.kind() == clap::error::ErrorKind::DisplayHelp {
                eprint!("{err}");
                std::process::exit(0);
            }
            err.exit()
        })
    }

    pub fn try_parse_from_compat<I, S>(args: I) -> Result<Self, clap::Error>
    where
        I: IntoIterator<Item = S>,
        S: Into<String>,
    {
        let mut argv: Vec<String> = args.into_iter().map(Into::into).collect();
        if argv.get(1).is_some_and(|arg| {
            matches!(
                arg.as_str(),
                "version" | "Version" | "cube-version" | "commit" | "help" | "--cube-version"
            )
        }) {
            if argv[1] == "--cube-version" {
                argv[1] = "cube-version".into();
            }
            return Self::try_parse_from(argv);
        }
        if argv.get(1).is_some_and(|arg| arg == "serve") {
            argv.remove(1);
        }
        // Go's flag package accepts both dash forms and only consumes a boolean
        // value when attached with '='. Parse every flag before choosing output.
        let mut normalized = vec![argv.first().cloned().unwrap_or_else(|| "cube-envd".into())];
        let mut arguments = argv.into_iter().skip(1);
        while let Some(arg) = arguments.next() {
            if arg == "--" || !arg.starts_with('-') || arg == "-" {
                break;
            }
            let (name, value) = arg
                .split_once('=')
                .map_or((arg.as_str(), None), |(n, v)| (n, Some(v)));
            let name = match name {
                "-cmd" => "--cmd",
                "-cgroup-root" => "--cgroup-root",
                "-port" => "--port",
                "-isnotfc" => "--isnotfc",
                "-version" => "--version",
                "-commit" => "--commit",
                name => name,
            };
            if let Some(value) = value {
                normalized.push(format!("{name}={value}"));
            } else if matches!(
                name,
                "--port" | "-p" | "--Port" | "--cmd" | "--cgroup-root" | "--log-format"
            ) {
                if let Some(value) = arguments.next() {
                    normalized.push(format!("{name}={value}"));
                } else {
                    normalized.push(name.into());
                }
            } else {
                normalized.push(name.into());
            }
        }
        let config = ServeConfig::try_parse_from(normalized)?;
        Ok(Self {
            command: if config.version {
                Command::Version
            } else if config.commit {
                Command::Commit
            } else {
                Command::Serve(config)
            },
        })
    }
}

fn parse_go_integer(value: &str) -> Result<i64, &'static str> {
    let (sign, unsigned) = match value.as_bytes().first() {
        Some(b'-') => ("-", &value[1..]),
        Some(b'+') => ("+", &value[1..]),
        _ => ("", value),
    };
    let (radix, digits, prefixed) = if unsigned.starts_with("0x") || unsigned.starts_with("0X") {
        (16, &unsigned[2..], true)
    } else if unsigned.starts_with("0b") || unsigned.starts_with("0B") {
        (2, &unsigned[2..], true)
    } else if unsigned.starts_with("0o") || unsigned.starts_with("0O") {
        (8, &unsigned[2..], true)
    } else if unsigned.starts_with('0') && unsigned.len() > 1 {
        (8, unsigned, false)
    } else {
        (10, unsigned, false)
    };
    let mut previous_digit = prefixed;
    for ch in digits.chars() {
        if ch == '_' {
            if !previous_digit {
                return Err("invalid integer value");
            }
            previous_digit = false;
        } else if ch.is_ascii() && ch.is_digit(radix) {
            previous_digit = true;
        } else {
            return Err("invalid integer value");
        }
    }
    if !previous_digit {
        return Err("invalid integer value");
    }
    i64::from_str_radix(&format!("{sign}{}", digits.replace('_', "")), radix)
        .map_err(|_| "invalid integer value")
}

fn parse_go_bool(value: &str) -> Result<bool, &'static str> {
    match value {
        "1" | "t" | "T" | "TRUE" | "true" | "True" => Ok(true),
        "0" | "f" | "F" | "FALSE" | "false" | "False" => Ok(false),
        _ => Err("invalid boolean value"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn accepts_legacy_server_flags() {
        let cli = Cli::try_parse_from_compat(["cube-envd", "-port", "1234", "-isnotfc"])
            .expect("legacy flags");
        let Command::Serve(config) = cli.command else {
            panic!("expected serve");
        };
        assert_eq!(config.port, 1234);
        assert!(config.is_not_fc);
    }

    #[test]
    fn accepts_legacy_version_flag() {
        assert!(matches!(
            Cli::try_parse_from_compat(["cube-envd", "-version"])
                .expect("version")
                .command,
            Command::Version
        ));
    }
}
