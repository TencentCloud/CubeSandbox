// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
// System command executor with timeout and error handling
//
// Wraps calls to external commands (rbd, ceph, ...) using
// `std::process::Command` with configurable timeout.

use std::process::Command;
use std::time::Duration;

use tracing::{debug, warn};

use crate::pkg::errors::{CubecowError, CubecowResult};

/// Default command execution timeout.
const DEFAULT_TIMEOUT: Duration = Duration::from_secs(30);

/// Abstraction over external command execution so engines that shell
/// out (e.g. the rbd backend) can be unit-tested with a scripted
/// runner instead of real binaries.
pub trait CommandRunner: Send + Sync {
    /// Execute `program` with `args`, returning stdout on success.
    fn run(&self, program: &str, args: &[&str]) -> CubecowResult<String>;
}

impl CommandRunner for CommandExecutor {
    fn run(&self, program: &str, args: &[&str]) -> CubecowResult<String> {
        CommandExecutor::run(self, program, args)
    }
}

/// Sync system command executor with timeout and structured error handling.
#[derive(Debug, Clone)]
pub struct CommandExecutor {
    timeout: Duration,
}

impl Default for CommandExecutor {
    fn default() -> Self {
        Self {
            timeout: DEFAULT_TIMEOUT,
        }
    }
}

impl CommandExecutor {
    /// Create a new executor with a custom timeout.
    pub fn new(timeout: Duration) -> Self {
        Self { timeout }
    }

    /// Execute a command and return its stdout on success.
    ///
    /// Returns `CubecowError::CommandFailed` on timeout or non-zero exit code.
    pub fn run(&self, program: &str, args: &[&str]) -> CubecowResult<String> {
        use std::io::Read;

        let cmd_str = format!("{} {}", program, args.join(" "));
        debug!(cmd = %cmd_str, "executing command");

        let mut child = Command::new(program)
            .args(args)
            .stdin(std::process::Stdio::null())
            .stdout(std::process::Stdio::piped())
            .stderr(std::process::Stdio::piped())
            .spawn()
            .map_err(|e| CubecowError::CommandFailed {
                cmd: cmd_str.clone(),
                reason: format!("failed to spawn: {e}"),
                code: None,
            })?;

        // Drain stdout/stderr on dedicated threads so output larger than
        // the pipe buffer cannot block the child and be mistaken for a
        // timeout.
        let mut stdout_pipe = child.stdout.take().expect("stdout is piped");
        let mut stderr_pipe = child.stderr.take().expect("stderr is piped");
        let stdout_reader = std::thread::spawn(move || {
            let mut buf = Vec::new();
            let _ = stdout_pipe.read_to_end(&mut buf);
            buf
        });
        let stderr_reader = std::thread::spawn(move || {
            let mut buf = Vec::new();
            let _ = stderr_pipe.read_to_end(&mut buf);
            buf
        });

        // Poll for exit or timeout without touching the pipes. On the
        // timeout and io-error paths the readers are deliberately not
        // joined: a grandchild that inherited the pipes could keep them
        // open past the kill and hang the join, and no output is needed
        // to report those failures.
        let start = std::time::Instant::now();
        let poll_interval = Duration::from_millis(50);
        let status = loop {
            match child.try_wait() {
                Ok(Some(status)) => break status,
                Ok(None) => {
                    if start.elapsed() >= self.timeout {
                        let _ = child.kill();
                        let _ = child.wait();
                        warn!(cmd = %cmd_str, timeout = ?self.timeout, "command timed out");
                        return Err(CubecowError::CommandFailed {
                            cmd: cmd_str,
                            reason: format!("timed out after {:?}", self.timeout),
                            code: None,
                        });
                    }
                    std::thread::sleep(poll_interval);
                }
                Err(e) => {
                    let _ = child.kill();
                    let _ = child.wait();
                    return Err(CubecowError::CommandFailed {
                        cmd: cmd_str,
                        reason: format!("io error: {e}"),
                        code: None,
                    });
                }
            }
        };

        // The child has exited, so its pipe write ends are closed and
        // the reader threads finish.
        let stdout = stdout_reader.join().unwrap_or_default();
        let stderr = stderr_reader.join().unwrap_or_default();

        if status.success() {
            debug!(cmd = %cmd_str, "command succeeded");
            Ok(String::from_utf8_lossy(&stdout).to_string())
        } else {
            let stderr = String::from_utf8_lossy(&stderr).to_string();
            let code_str = status
                .code()
                .map(|c| c.to_string())
                .unwrap_or_else(|| "signal".to_string());
            warn!(cmd = %cmd_str, exit_code = %code_str, stderr = %stderr, "command failed");
            Err(CubecowError::CommandFailed {
                cmd: cmd_str,
                reason: format!("exit code {code_str}: {stderr}"),
                code: status.code(),
            })
        }
    }
}
