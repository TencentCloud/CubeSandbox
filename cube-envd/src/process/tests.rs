// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

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
    registry.live.write().await.insert(41, handle.clone());
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
    // 收尾已写入回放记录并从 live 表摘除。
    assert!(registry.live.read().await.get(&41).is_none());
    let terminal = registry.terminal.lock().await;
    assert_eq!(terminal.len(), 1);
    assert_eq!(terminal[0].pid, 41);
    assert_eq!(terminal[0].tag.as_deref(), Some("reuse"));
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
    let (mut second, _ended) = fanout.subscribe();
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
    let (mut second, _ended) = fanout.subscribe();
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

/// 验证 seal 之后普通输出事件被丢弃；End 不经过队列，经 mark_ended 的 End 槽送达。
#[tokio::test]
async fn sealed_fanout_drops_output_but_still_delivers_end() {
    let (fanout, mut subscription) = OutputFanout::new();

    // seal 前的事件正常送达。
    fanout.send(ProcessEvent::Stdout(vec![1])).await;
    fanout.seal();

    // seal 后的普通输出被丢弃：send 立即返回且订阅者收不到它。
    let send = time::timeout(
        Duration::from_secs(1),
        fanout.send(ProcessEvent::Stdout(vec![2])),
    )
    .await;
    assert!(send.is_ok(), "sealed send must not block");

    // 收尾：登记 End 槽并关闭队列（同步操作，不等待订阅者）。
    fanout.mark_ended(&ProcessEvent::End(EndEvent {
        exit_code: 0,
        exited: true,
        status: "exit status 0".into(),
        error: None,
    }));
    fanout.close();

    // 订阅者依次收到：seal 前的 Stdout，然后是 End——中间没有 seal 后的输出。
    match time::timeout(Duration::from_secs(1), subscription.recv())
        .await
        .expect("pre-seal stdout in time")
        .expect("event delivered")
    {
        ProcessEvent::Stdout(bytes) => assert_eq!(bytes, vec![1]),
        _other => panic!("expected the pre-seal stdout first"),
    }
    match time::timeout(Duration::from_secs(1), subscription.recv())
        .await
        .expect("End in time")
        .expect("event delivered")
    {
        ProcessEvent::End(end) => assert!(end.exited),
        _other => panic!("expected End after queue drain"),
    }
    // seal 后不应再有第四个事件：stdout 在 seal 后已被丢弃，End 后流正常结束。
    assert!(
        time::timeout(Duration::from_millis(100), subscription.recv())
            .await
            .expect("stream must terminate cleanly")
            .is_none(),
        "no event may follow End"
    );
}

fn fake_end() -> ProcessEvent {
    ProcessEvent::End(EndEvent {
        exit_code: 0,
        exited: true,
        status: "exit status 0".into(),
        error: None,
    })
}

/// 回归：订阅者队列满载且停读时，mark_ended + close 不阻塞（进程收尾不被
/// 慢订阅者卡死），且订阅者排空队列后仍能收到 End。
#[tokio::test]
async fn end_reaches_a_subscriber_whose_queue_is_full_and_unread() {
    let (fanout, mut subscription) = OutputFanout::new();
    let total = crate::process::model::SUBSCRIBER_CAPACITY * 2;
    let producer_fanout = fanout.clone();
    let producer = tokio::spawn(async move {
        for index in 0..total {
            producer_fanout
                .send(ProcessEvent::Stdout(vec![index as u8]))
                .await;
        }
    });

    // 订阅者刻意不消费：生产者把队列填满（SUBSCRIBER_CAPACITY 条）并阻塞在
    // 其后的事件上。30ms 远大于生产者填满队列所需时间，保证此时正处于阻塞态。
    time::sleep(Duration::from_millis(30)).await;

    // 收尾与订阅者队列状态无关：必须立即完成（外层超时防回归挂死）。
    let ended = time::timeout(Duration::from_secs(2), async {
        fanout.mark_ended(&fake_end());
        fanout.close();
        // 现在开始消费：已入队的全部数据先到（capacity 条 + 阻塞在途 1 条，
        // 在我们腾出槽位后完成投递），随后队列排空、End 槽弹出 End。
        let mut received = 0usize;
        loop {
            match subscription.recv().await {
                Some(ProcessEvent::Stdout(_)) => received += 1,
                Some(ProcessEvent::End(end)) => return (received, end),
                Some(_other) => panic!("unexpected event"),
                None => panic!("stream ended without End"),
            }
        }
    })
    .await
    .expect("mark_ended/close must not block on a full subscriber queue");

    // close() 之后（现实收尾路径中 reader 已被 join/abort，不会有后续 send；
    // 此处是人工让生产者继续发送的极端情形）的投递按"无订阅者即丢弃"处理，
    // 因此只应收到：已缓冲的 capacity 条 + 收尾时在途阻塞的那 1 条。
    assert_eq!(
        ended.0,
        crate::process::model::SUBSCRIBER_CAPACITY + 1,
        "buffered and in-flight output arrives before End"
    );
    assert!(ended.1.exited);
    producer.await.unwrap();
    assert!(
        subscription.recv().await.is_none(),
        "stream terminates after End"
    );
}

/// 回归：进程收尾之后（mark_ended 之后）才挂载的订阅者，能感知"已收尾"
/// 并在队列排空后拿到 End，而不是得到一个永不结束/无 End 的空流。
#[tokio::test]
async fn subscriber_attaching_after_mark_ended_still_gets_end() {
    let (fanout, mut first) = OutputFanout::new();
    fanout.mark_ended(&fake_end());

    let (mut late, ended) = fanout.subscribe();
    assert!(ended, "late subscribe must observe the committed End");
    fanout.close();

    match time::timeout(Duration::from_secs(1), late.recv())
        .await
        .expect("late subscriber End in time")
        .expect("End delivered")
    {
        ProcessEvent::End(end) => assert!(end.exited),
        _other => panic!("expected End"),
    }
    assert!(late.recv().await.is_none());
    // 收尾前挂载的订阅者同样在排空后收到 End。
    match time::timeout(Duration::from_secs(1), first.recv())
        .await
        .expect("early subscriber End in time")
        .expect("End delivered")
    {
        ProcessEvent::End(end) => assert!(end.exited),
        _other => panic!("expected End"),
    }
}

/// 结束排空阶段：停读订阅者按 EOL 预算被放弃，健康订阅者仍收到全部输出。
/// 此处 begin_eol 在生产者已进入"活进程阶段的无预算阻塞投递"后才调用，
/// 验证阻塞中的投递能被 EOL 唤醒信号改按预算执行，而不是把 reader
/// 拖到 seal/abort 截尾。
#[tokio::test]
async fn eol_budget_abandons_a_wedged_subscriber_without_losing_others_output() {
    let (fanout, mut healthy) = OutputFanout::new();
    let (_wedged, _ended) = fanout.subscribe(); // 停读：从不 poll

    let total = crate::process::model::SUBSCRIBER_CAPACITY + 8;
    let producer_fanout = fanout.clone();
    let producer = tokio::spawn(async move {
        for index in 0..total {
            producer_fanout
                .send(ProcessEvent::Stdout(vec![index as u8]))
                .await;
        }
    });

    // 让生产者把停读订阅者的队列填满并阻塞在其后的投递上（活进程阶段，无预算）。
    time::sleep(Duration::from_millis(30)).await;
    // 子进程退出：进入结束排空，唤醒阻塞中的投递改按预算执行。
    fanout.begin_eol();

    let mut received = Vec::new();
    time::timeout(Duration::from_secs(5), async {
        while received.len() < total {
            match healthy.recv().await {
                Some(ProcessEvent::Stdout(bytes)) => received.push(bytes[0]),
                _other => panic!("unexpected event for healthy subscriber"),
            }
        }
    })
    .await
    .expect("healthy subscriber receives everything despite a wedged peer");
    producer.await.unwrap();
    assert_eq!(received, (0..total as u8).collect::<Vec<_>>());
}

/// 回归：finish 不会摘除已被同 PID 新进程占用的句柄，也不写入可能
/// 遮蔽新进程回放的陈旧记录；本进程自己的订阅者仍收到 End。
#[tokio::test]
async fn finish_does_not_evict_a_pid_reused_by_a_newer_process() {
    let registry = ProcessRegistry::default();
    let (output_a, mut subscriber_a) = OutputFanout::new();
    let handle_a = Arc::new(ProcessHandle {
        pid: 100,
        tag: Some("old".into()),
        config: ProcessConfig {
            cmd: "/bin/true".into(),
            args: Vec::new(),
            envs: HashMap::new(),
            cwd: None,
        },
        input: Mutex::new(ProcessInput::Closed),
        pty: None,
        output: output_a,
    });
    let (output_b, _subscriber_b) = OutputFanout::new();
    let handle_b = Arc::new(ProcessHandle {
        pid: 100,
        tag: Some("new".into()),
        config: ProcessConfig {
            cmd: "/bin/true".into(),
            args: Vec::new(),
            envs: HashMap::new(),
            cwd: None,
        },
        input: Mutex::new(ProcessInput::Closed),
        pty: None,
        output: output_b,
    });
    registry.live.write().await.insert(100, handle_b.clone());
    registry.tags.write().await.insert("old".into(), 100);

    registry
        .finish(
            handle_a,
            ProcessEvent::End(EndEvent {
                exit_code: 9,
                exited: true,
                status: "exit status 9".into(),
                error: None,
            }),
        )
        .await;

    // 新进程 B 仍在 live 表中，且 terminal 中没有 A 的陈旧记录。
    let owner = registry
        .live
        .read()
        .await
        .get(&100)
        .cloned()
        .expect("B live");
    assert!(Arc::ptr_eq(&owner, &handle_b), "B must not be evicted");
    assert!(
        registry.terminal.lock().await.is_empty(),
        "no stale record for a pid owned by a newer process"
    );
    // A 自己的订阅者仍收到 A 的 End。
    match time::timeout(Duration::from_secs(1), subscriber_a.recv())
        .await
        .expect("subscriber A End in time")
        .expect("End delivered")
    {
        ProcessEvent::End(end) => assert_eq!(end.exit_code, 9),
        _other => panic!("expected A's End"),
    }
    // 旧标签解绑，避免按旧标签解析到 B。
    assert!(registry.tags.read().await.get("old").is_none());

    // B 正常收尾：terminal 只保留 B 的记录。
    registry
        .finish(
            handle_b,
            ProcessEvent::End(EndEvent {
                exit_code: 0,
                exited: true,
                status: "exit status 0".into(),
                error: None,
            }),
        )
        .await;
    let terminal = registry.terminal.lock().await;
    assert_eq!(terminal.len(), 1);
    assert_eq!(terminal[0].pid, 100);
    assert_eq!(terminal[0].tag.as_deref(), Some("new"));
}
