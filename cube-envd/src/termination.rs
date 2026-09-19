use serde::Serialize;
use std::{
    fs,
    path::{Path, PathBuf},
    process::ExitStatus,
};

#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub(crate) struct TerminationInfo {
    pub(crate) reason: TerminationReason,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub(crate) signal: Option<i32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub(crate) signal_name: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub(crate) core_dumped: Option<bool>,
}

#[derive(Clone, Copy, Debug, Serialize)]
#[serde(rename_all = "snake_case")]
pub(crate) enum TerminationReason {
    Exited,
    Signal,
    Timeout,
    Oom,
    Unknown,
}

impl TerminationInfo {
    pub(crate) fn exited() -> Self {
        Self {
            reason: TerminationReason::Exited,
            signal: None,
            signal_name: None,
            core_dumped: None,
        }
    }

    pub(crate) fn timeout() -> Self {
        Self {
            reason: TerminationReason::Timeout,
            signal: None,
            signal_name: None,
            core_dumped: None,
        }
    }

    pub(crate) fn unknown() -> Self {
        Self {
            reason: TerminationReason::Unknown,
            signal: None,
            signal_name: None,
            core_dumped: None,
        }
    }

    pub(crate) fn signal(signal: i32, core_dumped: bool, oom_killed: bool) -> Self {
        Self {
            reason: if oom_killed {
                TerminationReason::Oom
            } else {
                TerminationReason::Signal
            },
            signal: Some(signal),
            signal_name: Some(signal_name(signal)),
            core_dumped: Some(core_dumped),
        }
    }
}

#[cfg(unix)]
pub(crate) fn from_exit_status(status: ExitStatus, oom_killed: bool) -> TerminationInfo {
    use std::os::unix::process::ExitStatusExt;

    if let Some(signal) = status.signal() {
        TerminationInfo::signal(signal, status.core_dumped(), oom_killed)
    } else if status.code().is_some() {
        TerminationInfo::exited()
    } else {
        TerminationInfo::unknown()
    }
}

#[cfg(not(unix))]
pub(crate) fn from_exit_status(status: ExitStatus, _oom_killed: bool) -> TerminationInfo {
    if status.code().is_some() {
        TerminationInfo::exited()
    } else {
        TerminationInfo::unknown()
    }
}

pub(crate) fn signal_name(signal: i32) -> String {
    let name = match signal {
        1 => "SIGHUP",
        2 => "SIGINT",
        3 => "SIGQUIT",
        4 => "SIGILL",
        5 => "SIGTRAP",
        6 => "SIGABRT",
        7 => "SIGBUS",
        8 => "SIGFPE",
        9 => "SIGKILL",
        10 => "SIGUSR1",
        11 => "SIGSEGV",
        12 => "SIGUSR2",
        13 => "SIGPIPE",
        14 => "SIGALRM",
        15 => "SIGTERM",
        17 => "SIGCHLD",
        18 => "SIGCONT",
        19 => "SIGSTOP",
        20 => "SIGTSTP",
        21 => "SIGTTIN",
        22 => "SIGTTOU",
        _ => return format!("SIG{signal}"),
    };
    name.to_owned()
}

pub(crate) fn legacy_signal_name(signal: i32) -> String {
    let name = match signal {
        1 => "hangup",
        2 => "interrupt",
        3 => "quit",
        4 => "illegal instruction",
        5 => "trace trap",
        6 => "aborted",
        7 => "bus error",
        8 => "floating point exception",
        9 => "killed",
        10 => "user defined signal 1",
        11 => "segmentation fault",
        12 => "user defined signal 2",
        13 => "pipe write",
        14 => "alarm",
        15 => "terminated",
        17 => "child exited",
        18 => "continued",
        19 => "stopped",
        20 => "keyboard stop",
        21 => "background read",
        22 => "background write",
        _ => return format!("signal {signal}"),
    };
    name.to_owned()
}

#[derive(Debug)]
pub(crate) struct CgroupMemoryMonitor {
    counter_path: Option<PathBuf>,
    counter_before: Option<u64>,
}

impl CgroupMemoryMonitor {
    pub(crate) fn start() -> Self {
        let counter_path = memory_counter_path();
        let counter_before = counter_path.as_ref().and_then(read_memory_counter);
        Self {
            counter_path,
            counter_before,
        }
    }

    pub(crate) fn was_oom_killed(&self) -> bool {
        let Some(before) = self.counter_before else {
            return false;
        };
        self.counter_path
            .as_ref()
            .and_then(read_memory_counter)
            .is_some_and(|after| after > before)
    }
}

#[cfg(target_os = "linux")]
fn memory_counter_path() -> Option<PathBuf> {
    let cgroup = fs::read_to_string("/proc/self/cgroup").ok()?;
    memory_counter_path_from_cgroup(&cgroup, Path::new("/sys/fs/cgroup"))
}

#[cfg(target_os = "linux")]
fn memory_counter_path_from_cgroup(cgroup: &str, cgroup_root: &Path) -> Option<PathBuf> {
    if let Some(relative) = cgroup.lines().find_map(|line| {
        let (hierarchy, controllers, path) = cgroup_fields(line)?;
        (hierarchy == "0" && controllers.is_empty()).then_some(path.trim_start_matches('/'))
    }) {
        let path = cgroup_root.join(relative).join("memory.events");
        if path.is_file() {
            return Some(path);
        }
        let root_path = cgroup_root.join("memory.events");
        if root_path.is_file() {
            return Some(root_path);
        }
    }

    let relative = cgroup.lines().find_map(|line| {
        let (_, controllers, path) = cgroup_fields(line)?;
        controllers
            .split(',')
            .any(|controller| controller == "memory")
            .then_some(path.trim_start_matches('/'))
    })?;
    let memory_path = cgroup_root.join("memory").join(relative);
    for filename in ["memory.oom_control", "memory.failcnt"] {
        let path = memory_path.join(filename);
        if path.is_file() {
            return Some(path);
        }
    }
    for filename in ["memory.oom_control", "memory.failcnt"] {
        let root_path = cgroup_root.join("memory").join(filename);
        if root_path.is_file() {
            return Some(root_path);
        }
    }
    None
}

#[cfg(target_os = "linux")]
fn cgroup_fields(line: &str) -> Option<(&str, &str, &str)> {
    let mut fields = line.splitn(3, ':');
    Some((fields.next()?, fields.next()?, fields.next()?))
}

#[cfg(not(target_os = "linux"))]
fn memory_counter_path() -> Option<PathBuf> {
    None
}

fn read_memory_counter(path: &PathBuf) -> Option<u64> {
    if path.file_name().and_then(|name| name.to_str()) == Some("memory.failcnt") {
        return fs::read_to_string(path).ok()?.trim().parse().ok();
    }
    fs::read_to_string(path).ok()?.lines().find_map(|line| {
        let (key, value) = line.split_once(char::is_whitespace)?;
        (key == "oom_kill")
            .then(|| value.trim().parse().ok())
            .flatten()
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn signal_information_is_structured() {
        let info = TerminationInfo::signal(11, true, false);
        assert!(matches!(info.reason, TerminationReason::Signal));
        assert_eq!(info.signal, Some(11));
        assert_eq!(info.signal_name.as_deref(), Some("SIGSEGV"));
        assert_eq!(info.core_dumped, Some(true));
    }

    #[test]
    fn oom_takes_precedence_over_generic_sigkill() {
        let info = TerminationInfo::signal(9, false, true);
        assert!(matches!(info.reason, TerminationReason::Oom));
        assert_eq!(info.signal_name.as_deref(), Some("SIGKILL"));
    }

    #[test]
    fn memory_events_parser_reads_oom_kill() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("memory.events");
        std::fs::write(&path, "low 0\nome 1\noom_kill 4\n").unwrap();
        assert_eq!(read_memory_counter(&path), Some(4));
    }

    #[test]
    fn cgroup_v1_counter_parser_reads_failcnt() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("memory.failcnt");
        std::fs::write(&path, "7\n").unwrap();
        assert_eq!(read_memory_counter(&path), Some(7));
    }

    #[test]
    fn cgroup_v1_counter_parser_reads_oom_control() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("memory.oom_control");
        std::fs::write(&path, "oom_kill_disable 0\nunder_oom 0\noom_kill 3\n").unwrap();
        assert_eq!(read_memory_counter(&path), Some(3));
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn cgroup_v1_path_parser_reads_linux_proc_format() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory
            .path()
            .join("memory")
            .join("docker")
            .join("container-id")
            .join("memory.failcnt");
        std::fs::create_dir_all(path.parent().unwrap()).unwrap();
        std::fs::write(&path, "7\n").unwrap();

        let cgroup = "12:cpu,cpuacct:/docker/container-id\n5:memory:/docker/container-id\n";
        assert_eq!(
            memory_counter_path_from_cgroup(cgroup, directory.path()),
            Some(path)
        );
    }

    #[test]
    fn signal_names_cover_known_and_unknown() {
        assert_eq!(signal_name(9), "SIGKILL");
        assert_eq!(signal_name(11), "SIGSEGV");
        assert_eq!(signal_name(64), "SIG64");
        assert_eq!(legacy_signal_name(9), "killed");
        assert_eq!(legacy_signal_name(64), "signal 64");
    }

    #[test]
    fn termination_reason_serializes_snake_case() {
        let value = serde_json::to_value(TerminationInfo::timeout()).unwrap();
        assert_eq!(value["reason"], "timeout");
        assert!(value.get("signal").is_none());
    }
}
