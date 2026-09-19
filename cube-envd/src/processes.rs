use std::collections::HashMap;
use std::io;
use std::sync::{Arc, Mutex, OnceLock};
use tokio::io::AsyncWriteExt;
use tokio::process::ChildStdin;
use tokio::sync::Mutex as AsyncMutex;

/// A running process (or PTY) tracked so `process.Process/List` can report it.
#[derive(Clone, Debug)]
pub(crate) struct ProcessRecord {
    pub(crate) cmd: String,
    pub(crate) args: Vec<String>,
    pub(crate) envs: HashMap<String, String>,
    pub(crate) cwd: Option<String>,
    pub(crate) tag: Option<String>,
}

fn registry() -> &'static Mutex<HashMap<u32, ProcessRecord>> {
    static REGISTRY: OnceLock<Mutex<HashMap<u32, ProcessRecord>>> = OnceLock::new();
    REGISTRY.get_or_init(|| Mutex::new(HashMap::new()))
}

pub(crate) fn register(pid: u32, record: ProcessRecord) {
    registry()
        .lock()
        .expect("process registry lock poisoned")
        .insert(pid, record);
}

pub(crate) fn unregister(pid: u32) {
    registry()
        .lock()
        .expect("process registry lock poisoned")
        .remove(&pid);
}

pub(crate) fn list() -> Vec<(u32, ProcessRecord)> {
    let registry = registry().lock().expect("process registry lock poisoned");
    let mut items: Vec<(u32, ProcessRecord)> = registry
        .iter()
        .map(|(pid, record)| (*pid, record.clone()))
        .collect();
    items.sort_by_key(|(pid, _)| *pid);
    items
}

/// Resolve a process tag to its pid, mirroring the proto `ProcessSelector`.
pub(crate) fn pid_for_tag(tag: &str) -> Option<u32> {
    registry()
        .lock()
        .expect("process registry lock poisoned")
        .iter()
        .find(|(_, record)| record.tag.as_deref() == Some(tag))
        .map(|(pid, _)| *pid)
}

type SharedStdin = Arc<AsyncMutex<ChildStdin>>;

fn stdin_registry() -> &'static Mutex<HashMap<u32, SharedStdin>> {
    static STDIN: OnceLock<Mutex<HashMap<u32, SharedStdin>>> = OnceLock::new();
    STDIN.get_or_init(|| Mutex::new(HashMap::new()))
}

/// Keep the child's piped stdin so `StreamInput`/`CloseStdin` can drive it.
pub(crate) fn register_stdin(pid: u32, writer: ChildStdin) {
    stdin_registry()
        .lock()
        .expect("stdin registry lock poisoned")
        .insert(pid, Arc::new(AsyncMutex::new(writer)));
}

/// Write a chunk of stdin. Returns `false` when the process has no piped
/// stdin (either it opted out with `stdin:false` or it already exited).
///
/// The writer is shared behind an async mutex and never removed here, so two
/// overlapping `StreamInput` frames serialize instead of the second seeing a
/// missing writer.
pub(crate) async fn write_stdin(pid: u32, data: &[u8]) -> io::Result<bool> {
    let writer = stdin_registry()
        .lock()
        .expect("stdin registry lock poisoned")
        .get(&pid)
        .cloned();
    let Some(writer) = writer else {
        return Ok(false);
    };
    let mut writer = writer.lock().await;
    writer.write_all(data).await?;
    writer.flush().await?;
    Ok(true)
}

/// Drop the child's stdin, delivering EOF to the process. Returns whether a
/// writer was present.
pub(crate) fn close_stdin(pid: u32) -> bool {
    stdin_registry()
        .lock()
        .expect("stdin registry lock poisoned")
        .remove(&pid)
        .is_some()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn registry_tracks_and_lists_processes() {
        let pid = 4_000_001;
        unregister(pid);
        assert!(!list().iter().any(|(candidate, _)| *candidate == pid));

        register(
            pid,
            ProcessRecord {
                cmd: "/bin/true".to_owned(),
                args: vec!["-x".to_owned()],
                envs: HashMap::from([("A".to_owned(), "B".to_owned())]),
                cwd: Some("/tmp".to_owned()),
                tag: Some("tag-1".to_owned()),
            },
        );
        let found = list().into_iter().find(|(candidate, _)| *candidate == pid);
        let (_, record) = found.expect("registered process is listed");
        assert_eq!(record.cmd, "/bin/true");
        assert_eq!(record.args, vec!["-x".to_owned()]);
        assert_eq!(record.tag.as_deref(), Some("tag-1"));

        unregister(pid);
        assert!(!list().iter().any(|(candidate, _)| *candidate == pid));
    }

    #[tokio::test]
    async fn stdin_registry_reports_missing_writer() {
        let pid = 4_000_002;
        assert!(!write_stdin(pid, b"input").await.unwrap());
        assert!(!close_stdin(pid));
    }
}
