// Copyright (c) 2019 Ant Financial
//
// SPDX-License-Identifier: Apache-2.0
//

use crate::device::online_device;
use crate::linux_abi::*;
use crate::sandbox::Sandbox;
use crate::AGENT_CONFIG;
use slog::Logger;

use anyhow::{anyhow, Result};
use netlink_sys::{protocols, SocketAddr, TokioSocket};
use std::fmt::Debug;
use std::os::unix::io::FromRawFd;
use std::sync::Arc;
use tokio::select;
use tokio::sync::watch::Receiver;
use tokio::sync::Mutex;
use tracing::instrument;

// Convenience macro to obtain the scope logger
macro_rules! sl {
    () => {
        slog_scope::logger().new(o!("subsystem" => "uevent"))
    };
}

#[derive(Debug, Default, Clone, PartialEq, Eq)]
pub struct Uevent {
    pub action: String,
    pub devpath: String,
    pub devname: String,
    pub subsystem: String,
    seqnum: String,
    pub interface: String,
}

pub trait UeventMatcher: Sync + Send + Debug + 'static {
    fn is_match(&self, uev: &Uevent) -> bool;
}

impl Uevent {
    fn new(message: &str) -> Self {
        let mut msg_iter = message.split('\0');
        let mut event = Uevent::default();

        msg_iter.next(); // skip the first value
        for arg in msg_iter {
            let key_val: Vec<&str> = arg.splitn(2, '=').collect();
            if key_val.len() == 2 {
                match key_val[0] {
                    U_EVENT_ACTION => event.action = String::from(key_val[1]),
                    U_EVENT_DEV_NAME => event.devname = String::from(key_val[1]),
                    U_EVENT_SUB_SYSTEM => event.subsystem = String::from(key_val[1]),
                    U_EVENT_DEV_PATH => event.devpath = String::from(key_val[1]),
                    U_EVENT_SEQ_NUM => event.seqnum = String::from(key_val[1]),
                    U_EVENT_INTERFACE => event.interface = String::from(key_val[1]),
                    _ => (),
                }
            }
        }

        event
    }

    #[instrument]
    async fn process_add(&self, logger: &Logger, sandbox: &Arc<Mutex<Sandbox>>) {
        // Special case for memory hot-adds first
        let online_path = format!("{}/{}/online", SYSFS_DIR, &self.devpath);
        if online_path.starts_with(SYSFS_MEMORY_ONLINE_PATH) {
            let _ = online_device(online_path.as_ref()).map_err(|e| {
                error!(
                    *logger,
                    "failed to online device";
                    "device" => &self.devpath,
                    "error" => format!("{}", e),
                )
            });
            return;
        }

        let mut sb = sandbox.lock().await;

        // Record the event by sysfs path
        sb.uevent_map.insert(self.devpath.clone(), self.clone());

        // Notify watchers that are interested in the udev event.
        sb.uevent_watchers.iter_mut().for_each(move |watch| {
            if let Some((matcher, _)) = watch {
                if matcher.is_match(self) {
                    let (_, sender) = watch.take().unwrap();
                    let _ = sender.send(self.clone());
                }
            }
        })
    }

    #[instrument]
    async fn process_remove(&self, logger: &Logger, sandbox: &Arc<Mutex<Sandbox>>) {
        let mut sb = sandbox.lock().await;
        sb.uevent_map.remove(&self.devpath);
    }

    #[instrument]
    async fn process(&self, logger: &Logger, sandbox: &Arc<Mutex<Sandbox>>) {
        if self.action == U_EVENT_ACTION_ADD {
            return self.process_add(logger, sandbox).await;
        } else if self.action == U_EVENT_ACTION_REMOVE {
            return self.process_remove(logger, sandbox).await;
        }
    }
}

#[instrument]
pub async fn wait_for_uevent(
    sandbox: &Arc<Mutex<Sandbox>>,
    matcher: impl UeventMatcher,
) -> Result<Uevent> {
    let logprefix = format!("Waiting for {:?}", &matcher);

    info!(sl!(), "{}", logprefix);
    let mut sb = sandbox.lock().await;
    for uev in sb.uevent_map.values() {
        if matcher.is_match(uev) {
            info!(sl!(), "{}: found {:?} in uevent map", logprefix, &uev);
            return Ok(uev.clone());
        }
    }

    // If device is not found in the device map, hotplug event has not
    // been received yet, create and add channel to the watchers map.
    // The key of the watchers map is the device we are interested in.
    // Note this is done inside the lock, not to miss any events from the
    // global udev listener.
    let (tx, rx) = tokio::sync::oneshot::channel::<Uevent>();
    let idx = sb.uevent_watchers.len();
    sb.uevent_watchers.push(Some((Box::new(matcher), tx)));
    drop(sb); // unlock

    info!(sl!(), "{}: waiting on channel", logprefix);

    let hotplug_timeout = AGENT_CONFIG.read().await.hotplug_timeout;

    let uev = match tokio::time::timeout(hotplug_timeout, rx).await {
        Ok(v) => v?,
        Err(_) => {
            let mut sb = sandbox.lock().await;
            let matcher = sb.uevent_watchers[idx].take().unwrap().0;

            return Err(anyhow!(
                "Timeout after {:?} waiting for uevent {:?}",
                hotplug_timeout,
                &matcher
            ));
        }
    };

    info!(sl!(), "{}: found {:?} on channel", logprefix, &uev);
    Ok(uev)
}

#[instrument]
pub async fn watch_uevents(
    sandbox: Arc<Mutex<Sandbox>>,
    mut shutdown: Receiver<bool>,
    sender: tokio::sync::oneshot::Sender<()>,
) -> Result<()> {
    let sref = sandbox.clone();
    let s = sref.lock().await;
    let logger = s.logger.new(o!("subsystem" => "uevent"));

    // Unlock the sandbox to allow a successful shutdown
    drop(s);

    debug!(logger, "starting uevents handler");

    let mut socket;

    unsafe {
        let fd = libc::socket(
            libc::AF_NETLINK,
            libc::SOCK_DGRAM | libc::SOCK_CLOEXEC,
            protocols::NETLINK_KOBJECT_UEVENT as libc::c_int,
        );
        socket = TokioSocket::from_raw_fd(fd);
    }

    socket.bind(&SocketAddr::new(u32::MAX - 6666, 1))?;
    sender.send(()).unwrap();
    loop {
        select! {
            _ = shutdown.changed() => {
                info!(logger, "got shutdown request");
                break;
            }
            result = socket.recv_from_full() => {
                match result {
                    Err(e) => {
                        error!(logger, "failed to receive uevent"; "error" => format!("{}", e))
                    }
                    Ok((buf, addr)) => {
                        if addr.port_number() != 0 {
                            // not our netlink message
                            let err_msg = format!("{:?}", nix::Error::EBADMSG);
                            error!(logger, "receive uevent message failed"; "error" => err_msg);
                            continue;
                        }

                        let text = String::from_utf8(buf);
                        match text {
                            Err(e) => {
                                error!(logger, "failed to convert bytes to text"; "error" => format!("{}", e))
                            }
                            Ok(text) => {
                                let event = Uevent::new(&text);
                                debug!(logger, "got uevent message"; "event" => format!("{:?}", event));
                                event.process(&logger, &sandbox).await;
                            }
                        }
                    }
                }
            }
        }
    }

    Ok(())
}

// Used in the device module unit tests
#[cfg(test)]
pub(crate) fn spawn_test_watcher(sandbox: Arc<Mutex<Sandbox>>, uev: Uevent) {
    tokio::spawn(async move {
        loop {
            let mut sb = sandbox.lock().await;
            let uev = uev.clone();
            sb.uevent_watchers.iter_mut().for_each(move |watch| {
                if let Some((matcher, _)) = watch {
                    if matcher.is_match(&uev) {
                        let (_, sender) = watch.take().unwrap();
                        let _ = sender.send(uev.clone());
                    }
                }
            });

            drop(sb); // unlock
        }
    });
}

#[cfg(test)]
mod tests {
    use super::*;

    #[derive(Debug, Clone, Copy)]
    struct AlwaysMatch();

    impl UeventMatcher for AlwaysMatch {
        fn is_match(&self, _: &Uevent) -> bool {
            true
        }
    }

    #[tokio::test]
    async fn test_wait_for_uevent() {
        let uev = Uevent {
            action: crate::linux_abi::U_EVENT_ACTION_ADD.to_string(),
            subsystem: "test".to_string(),
            devpath: "/test/sysfs/path".to_string(),
            devname: "testdevname".to_string(),
            ..Default::default()
        };

        let matcher = AlwaysMatch();

        let logger = slog::Logger::root(slog::Discard, o!());
        let sandbox = Arc::new(Mutex::new(Sandbox::new(&logger).unwrap()));

        let mut sb = sandbox.lock().await;
        sb.uevent_map.insert(uev.devpath.clone(), uev.clone());
        drop(sb); // unlock

        let uev2 = wait_for_uevent(&sandbox, matcher).await;
        assert!(uev2.is_ok());
        assert_eq!(uev2.unwrap(), uev);

        let mut sb = sandbox.lock().await;
        sb.uevent_map.remove(&uev.devpath).unwrap();
        drop(sb); // unlock

        spawn_test_watcher(sandbox.clone(), uev.clone());

        let uev2 = wait_for_uevent(&sandbox, matcher).await;
        assert!(uev2.is_ok());
        assert_eq!(uev2.unwrap(), uev);
    }
}
