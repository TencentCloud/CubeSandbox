// Copyright (c) 2020 Ant Group
//
// SPDX-License-Identifier: Apache-2.0
//

use anyhow::{anyhow, Context, Result};
use eventfd::{eventfd, EfdFlags};
use nix::sys::eventfd;
use std::fs::{self, File};
use std::os::unix::io::{AsRawFd, FromRawFd};
use std::path::Path;

use crate::pipestream::PipeStream;
use futures::StreamExt as _;
use inotify::{Inotify, WatchMask};
use tokio::io::AsyncReadExt;
use tokio::sync::mpsc::{channel, Receiver};

// Convenience macro to obtain the scope logger
macro_rules! sl {
    () => {
        slog_scope::logger().new(o!("subsystem" => "cgroups_notifier"))
    };
}

pub async fn notify_oom(cid: &str, cg_dir: String) -> Result<Receiver<String>> {
    if cgroups::hierarchies::is_cgroup2_unified_mode() {
        return notify_on_oom_v2(cid, cg_dir).await;
    }
    notify_on_oom(cid, cg_dir).await
}

// get_value_from_cgroup parse cgroup file with `Flat keyed`
// and get the value of `key`.
// Flat keyed file format:
//   KEY0 VAL0\n
//   KEY1 VAL1\n
fn get_value_from_cgroup(path: &Path, key: &str) -> Result<i64> {
    let content = fs::read_to_string(path)?;
    info!(
        sl!(),
        "get_value_from_cgroup file: {:?}, content: {}", &path, &content
    );

    for line in content.lines() {
        let arr: Vec<&str> = line.split(' ').collect();
        if arr.len() == 2 && arr[0] == key {
            let r = arr[1].parse::<i64>()?;
            return Ok(r);
        }
    }
    Ok(0)
}

// notify_on_oom returns channel on which you can expect event about OOM,
// if process died without OOM this channel will be closed.
pub async fn notify_on_oom_v2(containere_id: &str, cg_dir: String) -> Result<Receiver<String>> {
    register_memory_event_v2(containere_id, cg_dir, "memory.events", "cgroup.events").await
}

async fn register_memory_event_v2(
    containere_id: &str,
    cg_dir: String,
    memory_event_name: &str,
    cgroup_event_name: &str,
) -> Result<Receiver<String>> {
    let event_control_path = Path::new(&cg_dir).join(memory_event_name);
    let cgroup_event_control_path = Path::new(&cg_dir).join(cgroup_event_name);
    info!(
        sl!(),
        "register_memory_event_v2 event_control_path: {:?}", &event_control_path
    );
    info!(
        sl!(),
        "register_memory_event_v2 cgroup_event_control_path: {:?}", &cgroup_event_control_path
    );

    let mut inotify = Inotify::init().context("Failed to initialize inotify")?;

    // watching oom kill
    let ev_wd = inotify.add_watch(&event_control_path, WatchMask::MODIFY)?;
    // Because no `unix.IN_DELETE|unix.IN_DELETE_SELF` event for cgroup file system, so watching all process exited
    let cg_wd = inotify.add_watch(&cgroup_event_control_path, WatchMask::MODIFY)?;

    info!(sl!(), "ev_wd: {:?}", ev_wd);
    info!(sl!(), "cg_wd: {:?}", cg_wd);

    let (sender, receiver) = channel(100);
    let containere_id = containere_id.to_string();

    tokio::spawn(async move {
        let mut buffer = [0; 32];
        let mut stream = inotify
            .event_stream(&mut buffer)
            .expect("create inotify event stream failed");

        while let Some(event_or_error) = stream.next().await {
            let event = event_or_error.unwrap();
            info!(
                sl!(),
                "container[{}] get event for container: {:?}", &containere_id, &event
            );
            // info!("is1: {}", event.wd == wd1);
            info!(sl!(), "event.wd: {:?}", event.wd);

            if event.wd == ev_wd {
                let oom = get_value_from_cgroup(&event_control_path, "oom_kill");
                if oom.unwrap_or(0) > 0 {
                    let _ = sender.send(containere_id.clone()).await.map_err(|e| {
                        error!(sl!(), "send containere_id failed, error: {:?}", e);
                    });
                    return;
                }
            } else if event.wd == cg_wd {
                let pids = get_value_from_cgroup(&cgroup_event_control_path, "populated");
                if pids.unwrap_or(-1) == 0 {
                    return;
                }
            }

            // When a cgroup is destroyed, an event is sent to eventfd.
            // So if the control path is gone, return instead of notifying.
            if !Path::new(&event_control_path).exists() {
                return;
            }
        }
    });

    Ok(receiver)
}

// notify_on_oom returns channel on which you can expect event about OOM,
// if process died without OOM this channel will be closed.
async fn notify_on_oom(cid: &str, dir: String) -> Result<Receiver<String>> {
    if dir.is_empty() {
        return Err(anyhow!("memory controller missing"));
    }

    register_memory_event(cid, dir, "memory.oom_control", "").await
}

async fn register_memory_event(
    cid: &str,
    cg_dir: String,
    event_name: &str,
    arg: &str,
) -> Result<Receiver<String>> {
    let path = Path::new(&cg_dir).join(event_name);
    let event_file = File::open(path.clone())?;

    let eventfd = eventfd(0, EfdFlags::EFD_CLOEXEC)?;

    let event_control_path = Path::new(&cg_dir).join("cgroup.event_control");

    let data = if arg.is_empty() {
        format!("{} {}", eventfd, event_file.as_raw_fd())
    } else {
        format!("{} {} {}", eventfd, event_file.as_raw_fd(), arg)
    };

    fs::write(&event_control_path, data)?;

    let mut eventfd_stream = unsafe { PipeStream::from_raw_fd(eventfd) };

    let (sender, receiver) = tokio::sync::mpsc::channel(100);
    let containere_id = cid.to_string();

    tokio::spawn(async move {
        loop {
            let sender = sender.clone();
            let mut buf = [0u8; 8];
            match eventfd_stream.read(&mut buf).await {
                Err(err) => {
                    warn!(sl!(), "failed to read from eventfd: {:?}", err);
                    return;
                }
                Ok(_) => match fs::read_to_string(path.clone()) {
                    Ok(content) => {
                        debug!(
                            sl!(),
                            "cgroup event for container: {}, path: {:?}, content: {:?}",
                            &containere_id,
                            &path,
                            content
                        );
                    }
                    Err(_) => {}
                },
            }

            // When a cgroup is destroyed, an event is sent to eventfd.
            // So if the control path is gone, return instead of notifying.
            if !Path::new(&event_control_path).exists() {
                return;
            }

            let _ = sender.send(containere_id.clone()).await.map_err(|e| {
                error!(sl!(), "send containere_id failed, error: {:?}", e);
            });
        }
    });

    Ok(receiver)
}
