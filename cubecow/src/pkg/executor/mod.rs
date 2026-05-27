// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
// System command executor with timeout and error handling
//
// Wraps calls to external commands (lvm2, mdadm, etc.)
// using `std::process::Command` with configurable timeout.

use std::process::Command;
use std::time::Duration;

use tracing::{debug, warn};

use crate::pkg::errors::{CubecowError, CubecowResult};

/// Default command execution timeout.
const DEFAULT_TIMEOUT: Duration = Duration::from_secs(30);

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
        let cmd_str = format!("{} {}", program, args.join(" "));
        debug!(cmd = %cmd_str, "executing command");

        let mut child = Command::new(program)
            .args(args)
            .stdout(std::process::Stdio::piped())
            .stderr(std::process::Stdio::piped())
            .spawn()
            .map_err(|e| CubecowError::CommandFailed {
                cmd: cmd_str.clone(),
                reason: format!("failed to spawn: {e}"),
            })?;

        // Wait with timeout using a polling approach
        let start = std::time::Instant::now();
        let poll_interval = Duration::from_millis(50);
        loop {
            match child.try_wait() {
                Ok(Some(_status)) => {
                    // Process has exited, collect output
                    let output =
                        child
                            .wait_with_output()
                            .map_err(|e| CubecowError::CommandFailed {
                                cmd: cmd_str.clone(),
                                reason: format!("io error: {e}"),
                            })?;

                    if output.status.success() {
                        let stdout = String::from_utf8_lossy(&output.stdout).to_string();
                        debug!(cmd = %cmd_str, "command succeeded");
                        return Ok(stdout);
                    } else {
                        let stderr = String::from_utf8_lossy(&output.stderr).to_string();
                        let code = output
                            .status
                            .code()
                            .map(|c| c.to_string())
                            .unwrap_or_else(|| "signal".to_string());
                        warn!(cmd = %cmd_str, exit_code = %code, stderr = %stderr, "command failed");
                        return Err(CubecowError::CommandFailed {
                            cmd: cmd_str,
                            reason: format!("exit code {code}: {stderr}"),
                        });
                    }
                }
                Ok(None) => {
                    // Process still running, check timeout
                    if start.elapsed() >= self.timeout {
                        // Kill the process on timeout
                        let _ = child.kill();
                        let _ = child.wait();
                        warn!(cmd = %cmd_str, timeout = ?self.timeout, "command timed out");
                        return Err(CubecowError::CommandFailed {
                            cmd: cmd_str,
                            reason: format!("timed out after {:?}", self.timeout),
                        });
                    }
                    std::thread::sleep(poll_interval);
                }
                Err(e) => {
                    return Err(CubecowError::CommandFailed {
                        cmd: cmd_str,
                        reason: format!("io error: {e}"),
                    });
                }
            }
        }
    }
}
