use std::os::fd::RawFd;
use std::path::PathBuf;
use std::sync::mpsc::Sender;

use event_notifier::NotifyEvent;
use log::LevelFilter;
use seccompiler::SeccompAction;

#[allow(dead_code)]
#[derive(Clone, Debug)]
pub struct VmmConfig {
    pub core_dump: Option<CoreDumpConfig>,
    pub event_monitor: Option<EventMonitorConfig>,
    pub gdb: Option<GdbConfig>,
    pub log_file: String,
    pub log_json_file: Option<String>,
    pub log_level: LevelFilter,
    pub log_stderr: bool,
    pub sandbox_id: String,
    pub seccomp: SeccompAction,
    pub event_notifier: Option<EventNotifyConfig>,
    pub http_path: Option<String>,
}

impl Default for VmmConfig {
    fn default() -> Self {
        Self {
            core_dump: None,
            event_monitor: None,
            gdb: None,
            log_file: String::from(crate::common::DEFAULT_LOG_FILE),
            log_json_file: Some(String::from(crate::common::DEFAULT_LOG_JSON_FILE)),
            log_level: LevelFilter::Info,
            log_stderr: false,
            sandbox_id: String::from("cube-hypervisor"),
            seccomp: SeccompAction::KillProcess,
            event_notifier: None,
            http_path: None,
        }
    }
}

#[allow(dead_code)]
#[derive(Clone, Debug)]
pub struct GdbConfig {
    path: PathBuf,
}

#[derive(Clone, Debug)]
pub struct CoreDumpConfig {
    pub filter: String,
    pub limit: Option<u64>,
}

#[derive(Clone, Debug)]
pub struct EventMonitorConfig {
    pub path: Option<String>,
    pub fd: Option<RawFd>,
}

#[derive(Clone, Debug)]
pub struct EventNotifyConfig {
    pub notifier: Sender<NotifyEvent>,
}
