use std::{collections::HashMap, sync::Arc, time::Duration};

use tokio::{sync::Mutex, time};

use super::{
    fanout::{OutputFanout, OutputSubscription},
    model::{
        EndEvent, ProcessConfig, ProcessEvent, ProcessHandle, ProcessInput, ProcessRegistry,
        Selector, TerminalRecord, TERMINAL_CACHE_TTL,
    },
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
    let (output, _) = OutputFanout::new();
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

/// 验证慢订阅者在超过队列容量时依然一个事件不丢（背压而非 Lagged）。
#[tokio::test]
async fn slow_subscriber_receives_every_event_losslessly() {
    let (fanout, mut subscription) = OutputFanout::new();
    let total = crate::process::model::SUBSCRIBER_CAPACITY * 4 + 3;
    let producer = tokio::spawn(async move {
        for index in 0..total {
            fanout.send(ProcessEvent::Stdout(vec![index as u8])).await;
        }
    });

    // 刻意慢消费：先睡让生产者填满队列，再逐个接收。
    time::sleep(Duration::from_millis(20)).await;
    let mut received = Vec::new();
    while let Some(event) = subscription.recv().await {
        received.push(event);
        if received.len() == total {
            break;
        }
    }
    producer.await.unwrap();

    assert_eq!(received.len(), total);
    for (index, event) in received.into_iter().enumerate() {
        match event {
            ProcessEvent::Stdout(bytes) => assert_eq!(bytes, vec![index as u8]),
            _other => panic!("unexpected event"),
        }
    }
}

/// 验证订阅者断开后生产者不被卡住，其余订阅者仍正常收到事件。
#[tokio::test]
async fn disconnected_subscriber_does_not_wedge_the_producer() {
    let (fanout, subscription) = OutputFanout::new();
    let mut second = fanout.subscribe();
    drop(subscription); // 第一个订阅者断开

    // 若 send 被死订阅者卡住，这里会超时失败。
    let send = time::timeout(
        Duration::from_secs(1),
        fanout.send(ProcessEvent::Stdout(vec![1])),
    )
    .await;
    assert!(send.is_ok(), "send must not block on a dead subscriber");

    let event = time::timeout(Duration::from_secs(1), second.recv())
        .await
        .expect("live subscriber receives in time")
        .expect("event delivered");
    assert!(matches!(event, ProcessEvent::Stdout(_)));
}

/// 验证无订阅者时投递直接丢弃、不会挂起生产者。
#[tokio::test]
async fn send_without_subscribers_drops_the_event_without_blocking() {
    let (fanout, subscription) = OutputFanout::new();
    drop(subscription);

    let send = time::timeout(
        Duration::from_secs(1),
        fanout.send(ProcessEvent::Stderr(vec![2])),
    )
    .await;
    assert!(
        send.is_ok(),
        "send must return immediately without subscribers"
    );
}

/// 验证多个订阅者各自收到完整且顺序一致的事件流。
#[tokio::test]
async fn multiple_subscribers_receive_identical_ordered_events() {
    let (fanout, mut first) = OutputFanout::new();
    let mut second = fanout.subscribe();
    let producer = tokio::spawn(async move {
        for value in 0..16u8 {
            fanout.send(ProcessEvent::Stdout(vec![value])).await;
        }
    });

    async fn collect(subscription: &mut OutputSubscription, expected: usize) -> Vec<u8> {
        let mut values = Vec::new();
        for _ in 0..expected {
            match time::timeout(Duration::from_secs(1), subscription.recv())
                .await
                .expect("event in time")
                .expect("event delivered")
            {
                ProcessEvent::Stdout(bytes) => values.push(bytes[0]),
                _other => panic!("unexpected event"),
            }
        }
        values
    }
    // 消费与生产必须并行：背压下队列满时 send 会等待订阅者腾出空间。
    let (a, b) = tokio::join!(collect(&mut first, 16), collect(&mut second, 16));
    producer.await.unwrap();
    assert_eq!(a, (0..16).collect::<Vec<_>>());
    assert_eq!(b, a);
}
