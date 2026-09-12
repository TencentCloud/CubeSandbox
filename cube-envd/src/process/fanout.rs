// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use std::{
    sync::{
        atomic::{AtomicBool, Ordering},
        Arc, Mutex as StdMutex,
    },
    task::{Context, Poll},
    time::Instant,
};

use futures_util::future::join_all;
use tokio::sync::{mpsc, watch};

use super::model::{ProcessEvent, EOL_SEND_BUDGET, SUBSCRIBER_CAPACITY};

#[derive(Clone)]
/// 多订阅者输出扇出器：并发投递、背压到生产者、无订阅者时丢弃。
///
/// 语义对齐上游 Go envd 的 MultiplexedChannel（见 e2b-dev/infra 的
/// packages/envd/internal/services/process/handler/multiplex.go）：
/// - 订阅者队列满时 `send` 挂起，压力沿 reader → OS pipe → 子进程向上传导，一个字节不丢；
/// - 每个事件对所有订阅者**并发**投递：单个停读订阅者不会饿死同一事件的其他订阅者，
///   只在其自身队列填满后通过背压链最终限制子进程（与参考实现的有界 Source 语义一致）；
/// - 没有订阅者时直接丢弃，避免无人消费时把进程卡死；
/// - 订阅者断开（receiver drop）时投递立即失败，发送后统一从注册表清理。
///
/// 结束语义（End 槽）：
/// - End 事件**不经过订阅者队列**：进程收尾时 `mark_ended` 把 End 登记到共享槽位，
///   订阅者在自己的队列排空（收到 `None`）后从槽位弹出 End 再结束流。因此任何订阅者
///   （包括永久停读、队列永远满的）都无法阻塞 End 发布与注册表收尾。
/// - `begin_eol` 进入结束排空阶段：子进程已退出、reader 正在冲刷尾部输出。此阶段对
///   订阅者的每次投递受 `EOL_SEND_BUDGET` 约束，超时即把该订阅者标记为 `abandoned`
///   并跳过其后续输出（其已缓冲的数据与 End 槽仍会送达，恢复读取后可干净收尾）。
/// - `seal` 在孙进程仍持有管道写端、reader 被中断时调用：置位后拒绝一切后续事件，
///   保证"输出到此为止"的截断点语义。
pub(crate) struct OutputFanout {
    inner: Arc<FanoutInner>,
}

struct FanoutInner {
    /// 活跃订阅者注册表：append-only 写入，断开/放弃时惰性清理。
    subscribers: StdMutex<Vec<Arc<SubscriberState>>>,
    /// 封条：置位后拒绝一切新事件的投递。
    sealed: AtomicBool,
    /// 结束排空阶段标志：置位后投递受 EOL_SEND_BUDGET 约束。
    end_of_life: AtomicBool,
    /// 结束排空阶段的唤醒信号：置位时唤醒所有处于"活进程阶段无预算阻塞投递"
    /// 的 deliver，让它们改按 EOL_SEND_BUDGET 重试（停读订阅者由此被放弃，
    /// 不会把 reader 拖到 seal/abort 截尾）。
    eol: watch::Sender<()>,
    /// 已提交的 End 事件（`mark_ended` 写入一次）。晚到的订阅者据此自填 End 槽，
    /// 保证"attach 与收尾交错"的任意时序下订阅者流都能以 End 收尾。
    end: StdMutex<Option<ProcessEvent>>,
}

struct SubscriberState {
    /// 订阅者队列发送端。
    tx: mpsc::Sender<ProcessEvent>,
    /// 该订阅者共享的 End 槽：队列排空后由 `OutputSubscription::poll_recv` 弹出。
    end_slot: Arc<StdMutex<Option<ProcessEvent>>>,
    /// 结束排空阶段投递超时后置位：后续事件跳过该订阅者。
    abandoned: AtomicBool,
}

/// 进程输出订阅端，Start 流与 Connect 流各持一个。
///
/// Drop 时关闭队列接收端，使对该订阅者的阻塞投递立即返回错误。
pub(crate) struct OutputSubscription {
    rx: mpsc::Receiver<ProcessEvent>,
    end_slot: Arc<StdMutex<Option<ProcessEvent>>>,
}

impl OutputFanout {
    /// 创建扇出器并注册首个订阅者，把"无订阅者丢输出"的窗口压到最小。
    pub(crate) fn new() -> (OutputFanout, OutputSubscription) {
        let (eol, _) = watch::channel(());
        let inner = Arc::new(FanoutInner {
            subscribers: StdMutex::new(Vec::new()),
            sealed: AtomicBool::new(false),
            end_of_life: AtomicBool::new(false),
            eol,
            end: StdMutex::new(None),
        });
        let fanout = OutputFanout { inner };
        let (subscription, _) = fanout.subscribe();
        (fanout, subscription)
    }

    /// 注册新订阅者，仅接收注册之后产生的事件。
    ///
    /// 返回 `(订阅者, 进程是否已收尾)`。若进程已收尾（`mark_ended` 已完成），
    /// 调用方应放弃本次挂载并改用终端回放记录——此时记录必然已写入注册表。
    pub(crate) fn subscribe(&self) -> (OutputSubscription, bool) {
        let end_slot = Arc::new(StdMutex::new(None));
        let (tx, rx) = mpsc::channel(SUBSCRIBER_CAPACITY);
        let state = Arc::new(SubscriberState {
            tx,
            end_slot: Arc::clone(&end_slot),
            abandoned: AtomicBool::new(false),
        });
        {
            let mut guard = self.inner.subscribers.lock().expect("fanout subscribers");
            guard.push(state);
        }

        // 若 End 已提交（挂载与 mark_ended 交错），立即自填 End 槽，保证流以 End 收尾。
        let ended = {
            let guard = self.inner.end.lock().expect("fanout end slot");
            match guard.as_ref() {
                Some(end) => {
                    *end_slot.lock().expect("subscriber end slot") = Some(end.clone());
                    true
                }
                None => false,
            }
        };
        (OutputSubscription { rx, end_slot }, ended)
    }

    /// 并发阻塞投递一个事件给所有未放弃的订阅者；无订阅者时直接丢弃。
    ///
    /// 并发投递保证每个订阅者队列中的全局事件顺序一致（单个 reader 的 FIFO），
    /// 同时避免慢订阅者饿死同事件的其他订阅者。结束排空阶段（`begin_eol` 之后）
    /// 每次投递受 `EOL_SEND_BUDGET` 约束，超时即放弃该订阅者的后续输出。
    pub(crate) async fn send(&self, event: ProcessEvent) {
        if self.inner.sealed.load(Ordering::Relaxed) {
            return;
        }
        // 顺序敏感：先订阅唤醒通道再读标志位。若读到标志位已置位，说明
        // begin_eol 的 send() 必然发生在本订阅之后（begin_eol 先置位后通知），
        // deliver 的 live 分支不会错过唤醒；若标志位未置位，后续 begin_eol
        // 的通知必然能被本订阅者观察到。
        let eol = self.inner.eol.subscribe();
        let deadline = self
            .inner
            .end_of_life
            .load(Ordering::Relaxed)
            .then(|| Instant::now() + EOL_SEND_BUDGET);
        let subscribers: Vec<Arc<SubscriberState>> = {
            let guard = self.inner.subscribers.lock().expect("fanout subscribers");
            guard.clone()
        };
        join_all(
            subscribers
                .into_iter()
                .filter(|state| !state.abandoned.load(Ordering::Relaxed))
                .map(|state| deliver(state, event.clone(), deadline, eol.clone())),
        )
        .await;

        // 清理已断开（接收端已 drop）的订阅者。
        let mut guard = self.inner.subscribers.lock().expect("fanout subscribers");
        guard.retain(|state| !state.tx.is_closed());
    }

    /// 进入结束排空阶段：子进程已退出，后续投递受 EOL_SEND_BUDGET 约束；
    /// 同时唤醒所有处于无预算阻塞投递中的 deliver 改按预算重试。
    pub(crate) fn begin_eol(&self) {
        self.inner.end_of_life.store(true, Ordering::Relaxed);
        let _ = self.inner.eol.send(());
    }

    /// 封住输出：置位后一切后续事件被丢弃。
    ///
    /// 在孙进程仍持有管道写端、reader 被中断（pin 路径）后调用；End 不受影响——
    /// 它不经过队列，而是经 `mark_ended` 的 End 槽送达。
    pub(crate) fn seal(&self) {
        self.inner.sealed.store(true, Ordering::Relaxed);
    }

    /// 提交进程的 End 事件：登记共享 End 槽并填充每个当前订阅者的槽位。
    ///
    /// 只做同步登记，**永不等待订阅者排空**，因此停读订阅者无法阻塞收尾。
    /// 之后调用方应调用 `close()` 关闭订阅者队列；订阅者在其队列排空后
    /// （收到 `None`）从自己的槽位弹出 End，流以 End 帧收尾。
    pub(crate) fn mark_ended(&self, event: &ProcessEvent) {
        {
            let mut guard = self.inner.end.lock().expect("fanout end slot");
            if guard.is_none() {
                *guard = Some(event.clone());
            }
        }
        let subscribers: Vec<Arc<SubscriberState>> = {
            let guard = self.inner.subscribers.lock().expect("fanout subscribers");
            guard.clone()
        };
        for state in subscribers {
            let mut slot = state.end_slot.lock().expect("subscriber end slot");
            if slot.is_none() {
                *slot = Some(event.clone());
            }
        }
    }

    /// 关闭全部订阅者队列：消费端排空已入队事件后收到 `None`，
    /// 随后经 End 槽拿到 End 事件并结束流。
    pub(crate) fn close(&self) {
        self.inner
            .subscribers
            .lock()
            .expect("fanout subscribers")
            .clear();
    }
}

/// 向单个订阅者投递一个事件。
///
/// 结束排空阶段（或阻塞中被唤醒进入排空阶段）带预算：超时仍未投递成功则
/// 放弃该订阅者的后续输出。活进程阶段无预算（背压语义），但阻塞中若收到
/// `begin_eol` 的唤醒信号会立即改按预算重试，从而不会把 reader 拖到
/// seal/abort 截尾。
async fn deliver(
    state: Arc<SubscriberState>,
    event: ProcessEvent,
    mut deadline: Option<Instant>,
    mut eol: watch::Receiver<()>,
) {
    loop {
        if state.abandoned.load(Ordering::Relaxed) {
            return;
        }
        if let Some(deadline) = deadline {
            let now = Instant::now();
            if now >= deadline {
                state.abandoned.store(true, Ordering::Relaxed);
                return;
            }
            if tokio::time::timeout(deadline - now, state.tx.send(event.clone()))
                .await
                .is_ok()
            {
                return;
            }
            // 结束排空期内仍无法投递：订阅者停读，放弃其后续输出。
            // 其已缓冲的数据与 End 槽不受影响，恢复读取后仍可干净收尾。
            state.abandoned.store(true, Ordering::Relaxed);
            return;
        }
        tokio::select! {
            _ = state.tx.send(event.clone()) => return,
            changed = eol.changed() => {
                if changed.is_err() {
                    // 唤醒通道关闭（扇出器已销毁），不再等待。
                    return;
                }
                deadline = Some(Instant::now() + EOL_SEND_BUDGET);
            }
        }
    }
}

impl OutputSubscription {
    /// 从订阅队列取下一个事件；队列排空后如有 End 槽则返回 End，再之后返回 `None`。
    pub(crate) fn poll_recv(&mut self, context: &mut Context<'_>) -> Poll<Option<ProcessEvent>> {
        match self.rx.poll_recv(context) {
            Poll::Ready(Some(event)) => Poll::Ready(Some(event)),
            Poll::Pending => Poll::Pending,
            // 队列排空（fanout 已 close）：若进程已收尾，就地合成 End 事件。
            Poll::Ready(None) => {
                let end = self.end_slot.lock().ok().and_then(|mut slot| slot.take());
                match end {
                    Some(end) => Poll::Ready(Some(end)),
                    None => Poll::Ready(None),
                }
            }
        }
    }

    /// 异步接收下一个事件；队列关闭且 End 槽为空后返回 `None`。
    #[cfg(test)]
    pub(crate) async fn recv(&mut self) -> Option<ProcessEvent> {
        std::future::poll_fn(|context| self.poll_recv(context)).await
    }
}
