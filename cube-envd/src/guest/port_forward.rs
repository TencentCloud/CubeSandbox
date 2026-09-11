// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use std::collections::{BTreeMap, BTreeSet};
use std::os::unix::process::CommandExt;
use std::sync::Arc;
use std::time::Duration;

const SCAN_INTERVAL: Duration = Duration::from_secs(1);
const SOURCE_IP: &str = "169.254.0.21";

#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord)]
struct Listener {
    pid: i32,
    family: u8,
    port: u16,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
struct Socket {
    family: u8,
    port: u16,
    inode: u64,
}

pub(crate) async fn run(
    cgroup: Arc<crate::cgroup::ProcessCgroup>,
    mut shutdown: tokio::sync::watch::Receiver<bool>,
) -> Result<(), ()> {
    let mut forwards: BTreeMap<Listener, Option<Forward>> = BTreeMap::new();
    let mut interval = tokio::time::interval(SCAN_INTERVAL);
    interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    loop {
        tokio::select! {
            biased;
            changed = shutdown.changed() => {
                if changed.is_err() || *shutdown.borrow_and_update() {
                    cleanup(&mut forwards).await;
                    return Ok(());
                }
            }
            _ = interval.tick() => {
                match scan().await {
                    Ok(listeners) => refresh(&cgroup, &mut forwards, listeners).await,
                    Err(error) => tracing::warn!(errno = error.raw_os_error(), "localhost port scan failed"),
                }
            }
        }
    }
}

async fn scan() -> std::io::Result<BTreeSet<Listener>> {
    tokio::task::spawn_blocking(scan_sync)
        .await
        .map_err(|error| std::io::Error::other(format!("port scan task failed: {error}")))?
}

fn scan_sync() -> std::io::Result<BTreeSet<Listener>> {
    let mut sockets = Vec::new();
    for (path, family) in [("/proc/net/tcp", 4), ("/proc/net/tcp6", 6)] {
        let table = match std::fs::read_to_string(path) {
            Ok(table) => table,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => continue,
            Err(error) => return Err(error),
        };
        sockets.extend(parse(&table, family));
    }
    let owners = socket_owners();
    Ok(sockets
        .into_iter()
        .map(|socket| Listener {
            pid: owners.get(&socket.inode).copied().unwrap_or_default(),
            family: socket.family,
            port: socket.port,
        })
        .collect())
}

fn parse(table: &str, family: u8) -> Vec<Socket> {
    let loopback = if family == 4 {
        "0100007F"
    } else {
        "00000000000000000000000001000000"
    };
    table
        .lines()
        .skip(1)
        .filter_map(|line| {
            let mut fields = line.split_whitespace();
            fields.next()?;
            let local = fields.next()?;
            fields.next()?;
            if fields.next()? != "0A" {
                return None;
            }
            let (address, port) = local.split_once(':')?;
            if address != loopback {
                return None;
            }
            let inode = line.split_whitespace().nth(9)?.parse().ok()?;
            Some(Socket {
                family,
                port: u16::from_str_radix(port, 16).ok()?,
                inode,
            })
        })
        .collect()
}

fn socket_owners() -> BTreeMap<u64, i32> {
    let mut owners = BTreeMap::new();
    let Ok(processes) = std::fs::read_dir("/proc") else {
        return owners;
    };
    for process in processes.flatten() {
        let Some(pid) = process
            .file_name()
            .to_str()
            .and_then(|name| name.parse::<i32>().ok())
        else {
            continue;
        };
        let Ok(files) = std::fs::read_dir(process.path().join("fd")) else {
            continue;
        };
        for file in files.flatten() {
            let Ok(target) = std::fs::read_link(file.path()) else {
                continue;
            };
            let Some(inode) = target
                .to_str()
                .and_then(|target| target.strip_prefix("socket:["))
                .and_then(|target| target.strip_suffix(']'))
                .and_then(|inode| inode.parse::<u64>().ok())
            else {
                continue;
            };
            owners.entry(inode).or_insert(pid);
        }
    }
    owners
}

async fn refresh(
    cgroup: &crate::cgroup::ProcessCgroup,
    forwards: &mut BTreeMap<Listener, Option<Forward>>,
    listeners: BTreeSet<Listener>,
) {
    let stale: Vec<_> = forwards
        .keys()
        .filter(|listener| !listeners.contains(listener))
        .copied()
        .collect();
    for listener in stale {
        if let Some(Some(mut child)) = forwards.remove(&listener) {
            stop(&mut child).await;
        }
    }
    for listener in listeners {
        if let Some(child) = forwards.get_mut(&listener) {
            match child.as_ref().map(Forward::try_wait) {
                Some(Ok(None)) => continue,
                Some(Ok(Some(status))) => tracing::warn!(
                    pid = listener.pid,
                    port = listener.port,
                    family = listener.family,
                    %status,
                    "localhost forwarding exited; restarting"
                ),
                Some(Err(error)) => tracing::warn!(
                    errno = error.raw_os_error(),
                    pid = listener.pid,
                    port = listener.port,
                    family = listener.family,
                    "could not inspect localhost forwarding; restarting"
                ),
                None => {}
            }
        }
        if let Some(Some(mut child)) = forwards.remove(&listener) {
            stop(&mut child).await;
        }
        let child = start(cgroup, listener).await;
        forwards.insert(listener, child);
    }
}

async fn start(cgroup: &crate::cgroup::ProcessCgroup, listener: Listener) -> Option<Forward> {
    let mut command = tokio::process::Command::new("socat");
    command
        .args([
            "-d",
            "-d",
            "-d",
            &format!(
                "TCP4-LISTEN:{},bind={SOURCE_IP},reuseaddr,fork",
                listener.port
            ),
            &format!("TCP{}:localhost:{}", listener.family, listener.port),
        ])
        .stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .kill_on_drop(true);
    command.as_std_mut().process_group(0);
    let mut cgroup = match cgroup.file.try_clone() {
        Ok(file) => file,
        Err(error) => {
            tracing::warn!(errno = error.raw_os_error(), "could not clone socat cgroup");
            return None;
        }
    };
    unsafe {
        command.as_std_mut().pre_exec(move || {
            use std::io::Write;
            cgroup.write_all(b"0")
        });
    }
    match command.spawn() {
        Ok(child) => {
            tracing::debug!(
                pid = listener.pid,
                port = listener.port,
                family = listener.family,
                "started localhost forwarding"
            );
            Some(Forward {
                group: child.id().expect("spawned helper PID") as i32,
                child,
            })
        }
        Err(error) => {
            tracing::warn!(
                errno = error.raw_os_error(),
                port = listener.port,
                "could not start localhost forwarding"
            );
            None
        }
    }
}

struct Forward {
    // Keep the leader unreaped until its entire group has been signalled.
    // This also prevents PID reuse from redirecting cleanup to another group.
    group: i32,
    child: tokio::process::Child,
}

impl Forward {
    fn try_wait(&self) -> std::io::Result<Option<std::process::ExitStatus>> {
        use std::os::unix::process::ExitStatusExt;
        let mut info = std::mem::MaybeUninit::<libc::siginfo_t>::zeroed();
        let result = unsafe {
            libc::waitid(
                libc::P_PID,
                self.group as libc::id_t,
                info.as_mut_ptr(),
                libc::WEXITED | libc::WNOHANG | libc::WNOWAIT,
            )
        };
        if result != 0 {
            return Err(std::io::Error::last_os_error());
        }
        let info = unsafe { info.assume_init() };
        if unsafe { info.si_pid() } == 0 {
            return Ok(None);
        }
        let status = unsafe { info.si_status() };
        Ok(Some(std::process::ExitStatus::from_raw(
            if info.si_code == libc::CLD_EXITED {
                status << 8
            } else {
                status
            },
        )))
    }
}

impl Drop for Forward {
    fn drop(&mut self) {
        if self.group != 0 {
            unsafe {
                libc::kill(-self.group, libc::SIGKILL);
            }
        }
    }
}

async fn stop(forward: &mut Forward) {
    unsafe {
        libc::kill(-forward.group, libc::SIGKILL);
    }
    let _ = forward.child.wait().await;
    forward.group = 0;
}

async fn cleanup(forwards: &mut BTreeMap<Listener, Option<Forward>>) {
    for child in forwards.values_mut().filter_map(Option::as_mut) {
        stop(child).await;
    }
    forwards.clear();
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn scanner_selects_only_listening_loopback_tcp() {
        let table = "sl local_address rem_address st tx rx tr tm retr uid timeout inode\n\
          0: 0100007F:C351 00000000:0000 0A 0:0 00:0 0 1000 0 41\n\
          1: 00000000:C352 00000000:0000 0A 0:0 00:0 0 1000 0 42\n\
          2: 0100007F:C353 00000000:0000 01 0:0 00:0 0 1000 0 43\n";
        assert_eq!(
            parse(table, 4),
            vec![Socket {
                family: 4,
                port: 50001,
                inode: 41,
            }]
        );
    }
    #[test]
    fn scanner_selects_ipv6_loopback_without_requiring_ipv6_kernel_support() {
        let table = "sl local_address rem_address st tx rx tr tm retr uid timeout inode\n\
          0: 00000000000000000000000001000000:C351 00000000000000000000000000000000:0000 0A 0:0 00:0 0 1000 0 51\n\
          1: 00000000000000000000000000000000:C352 00000000000000000000000000000000:0000 0A 0:0 00:0 0 1000 0 52\n";
        assert_eq!(
            parse(table, 6),
            vec![Socket {
                family: 6,
                port: 50001,
                inode: 51
            }]
        );
    }
}
