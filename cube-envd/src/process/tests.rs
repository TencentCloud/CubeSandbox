use std::{
    collections::HashMap,
    sync::Arc,
    time::Duration,
};

use tokio::{
    sync::{broadcast, Mutex},
    time,
};

use super::model::{
    EndEvent, ProcessConfig, ProcessEvent, ProcessHandle, ProcessInput, ProcessRegistry,
    Selector, TERMINAL_CACHE_TTL, TerminalRecord,
};

/// 验证持有结束缓存锁时，标签查询会在锁释放后解析为缓存 PID。
#[tokio::test]
async fn resolves_terminal_tag_after_a_concurrent_terminal_cache_write() {
    let registry = ProcessRegistry::default();
    registry.terminal.lock().await.push(TerminalRecord {
        pid: 42,
        tag: Some("finished".into()),
        event: ProcessEvent::End(EndEvent {
            exit_code: 0,
            exited: true,
            status: "exit status 0".into(),
            error: None,
        }),
        expires: time::Instant::now() + TERMINAL_CACHE_TTL,
    });

    let terminal_write = registry.terminal.lock().await;
    let lookup_registry = registry.clone();
    let mut lookup = tokio::spawn(async move {
        lookup_registry
            .resolve_selector(Some(&Selector {
                pid: None,
                tag: Some("finished".into()),
            }))
            .await
    });

    tokio::task::yield_now().await;
    assert!(time::timeout(Duration::from_millis(25), &mut lookup)
        .await
        .is_err());
    drop(terminal_write);

    let pid = time::timeout(Duration::from_secs(1), lookup)
        .await
        .expect("terminal cache lock is released")
        .expect("lookup task completes")
        .expect("terminal tag resolves");
    assert_eq!(pid, 42);
}

/// 验证结束记录写入前不会释放标签，从而避免新旧进程标签竞态。
#[tokio::test]
async fn finish_keeps_a_tag_reserved_until_its_terminal_record_is_written() {
    let registry = ProcessRegistry::default();
    let (output, _) = broadcast::channel(1);
    let handle = Arc::new(ProcessHandle {
        pid: 41,
        tag: Some("reuse".into()),
        config: ProcessConfig {
            cmd: "/bin/true".into(),
            args: Vec::new(),
            envs: HashMap::new(),
            cwd: None,
        },
        input: Mutex::new(ProcessInput::Closed),
        pty: None,
        output,
    });
    registry.tags.write().await.insert("reuse".into(), 41);
    let terminal_write = registry.terminal.lock().await;
    let finish_registry = registry.clone();
    let finish = tokio::spawn(async move {
        finish_registry
            .finish(
                handle,
                ProcessEvent::End(EndEvent {
                    exit_code: 0,
                    exited: true,
                    status: "exit status 0".into(),
                    error: None,
                }),
            )
            .await;
    });

    tokio::task::yield_now().await;
    let mut new_reservation = tokio::spawn({
        let registry = registry.clone();
        async move { registry.tags.write().await.insert("reuse".into(), 0) }
    });
    assert!(
        time::timeout(Duration::from_millis(25), &mut new_reservation)
            .await
            .is_err()
    );

    drop(terminal_write);
    finish.await.unwrap();
    let previous = time::timeout(Duration::from_secs(1), new_reservation)
        .await
        .expect("tag reservation lock is released")
        .expect("reservation task completes");
    assert_eq!(previous, None);
}
