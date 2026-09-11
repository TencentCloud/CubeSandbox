// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Low-level process-group signalling used for execution cleanup.

/// Signal an entire process group started by `spawn` (which puts each child in
/// its own group with pgid == pid). Sending to `-pid` reaches the child and any
/// descendants it forked, so a timeout or SendSignal cleans up the whole tree
/// instead of orphaning grandchildren.
///
/// Refuses pid <= 1: `kill(0, …)`/`kill(-0, …)` would target envd's OWN process
/// group and `kill(-1, …)` every process the daemon may signal, either of which
/// would take envd itself down. A spawned child never legitimately has such a
/// pid; a bogus one is dropped rather than acted on.
pub fn kill_process_group(pid: u32, signo: i32) -> std::io::Result<()> {
    if pid <= 1 {
        return Err(std::io::Error::new(
            std::io::ErrorKind::InvalidInput,
            format!("refusing to signal pid {pid}"),
        ));
    }
    let rc = unsafe { libc::kill(-(pid as libc::pid_t), signo) };
    if rc != 0 {
        return Err(std::io::Error::last_os_error());
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::process::engine::tests::current_user;
    use crate::process::engine::{spawn, PumpEvent};
    use std::collections::HashMap;

    #[tokio::test]
    async fn signal_end_event_shape() {
        let user = current_user();
        let mut proc = spawn(
            "/bin/sh",
            &["-c".into(), "sleep 30".into()],
            HashMap::new(),
            "/".into(),
            &user,
            false,
            None,
        )
        .unwrap();
        tokio::time::sleep(std::time::Duration::from_millis(100)).await;
        kill_process_group(proc.pid, libc::SIGKILL).unwrap();
        let mut end = None;
        loop {
            match proc.initial.recv().await {
                Ok(PumpEvent::End(e)) => {
                    end = Some(e);
                    break;
                }
                Ok(_) => {}
                Err(_) => break,
            }
        }
        let end = end.unwrap();
        assert_eq!(end.exit_code, -1);
        assert!(!end.exited);
        assert_eq!(end.status, "signal: killed");
    }

    #[tokio::test]
    async fn kill_process_group_refuses_low_pids() {
        assert!(kill_process_group(0, libc::SIGKILL).is_err());
        assert!(kill_process_group(1, libc::SIGKILL).is_err());
    }
}
