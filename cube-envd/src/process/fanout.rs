use std::{
    sync::{
        atomic::{AtomicBool, Ordering},
        Arc, Mutex as StdMutex,
    },
    task::{Context, Poll},
};

use tokio::sync::mpsc;

use super::model::{ProcessEvent, SUBSCRIBER_CAPACITY};

#[derive(Clone)]
/// 多订阅者输出扇出器：阻塞投递、背压到生产者、无订阅者时丢弃。
///
/// 语义对齐上游 Go envd 的 MultiplexedChannel（见 e2b-dev/infra 的
/// packages/envd/internal/services/process/handler/multiplex.go）：
/// - 订阅者队列满时 `send` 挂起，压力沿 reader → OS pipe → 子进程向上传导，一个字节不丢；
/// - 没有订阅者时直接丢弃，避免无人消费时把进程卡死；
/// - 订阅者断开（receiver drop）时投递立即失败，惰性从注册表移除。
///
/// `seal` 在子进程已被回收而孙进程仍持有管道写端时调用：此后普通输出
/// 事件被丢弃，只有 End 事件仍可投递，保证 End 一定是订阅者队列的尾事件。
pub(crate) struct OutputFanout {
    inner: Arc<FanoutInner>,
}

struct FanoutInner {
    /// 活跃订阅者注册表：append-only 写入，断开时惰性移除。
    subscribers: StdMutex<Vec<Subscriber>>,
    /// 封条：置位后拒绝非 End 输出事件。
    sealed: AtomicBool,
}

struct Subscriber {
    /// 订阅者队列发送端；`Arc` 指针用于投递失败后定位并移除死订阅者。
    tx: Arc<mpsc::Sender<ProcessEvent>>,
}

/// 进程输出订阅端，Start 流与 Connect 流各持一个。
///
/// Drop 时关闭队列接收端，使对该订阅者的阻塞投递立即返回错误。
pub(crate) struct OutputSubscription {
    rx: mpsc::Receiver<ProcessEvent>,
}

impl OutputFanout {
    /// 创建扇出器并注册首个订阅者，把"无订阅者丢输出"的窗口压到最小。
    pub(crate) fn new() -> (OutputFanout, OutputSubscription) {
        let inner = Arc::new(FanoutInner {
            subscribers: StdMutex::new(Vec::new()),
            sealed: AtomicBool::new(false),
        });
        let fanout = OutputFanout { inner };
        let subscription = fanout.subscribe();
        (fanout, subscription)
    }

    /// 注册新订阅者，仅接收注册之后产生的事件。
    pub(crate) fn subscribe(&self) -> OutputSubscription {
        let (tx, rx) = mpsc::channel(SUBSCRIBER_CAPACITY);
        self.inner
            .subscribers
            .lock()
            .unwrap()
            .push(Subscriber { tx: Arc::new(tx) });
        OutputSubscription { rx }
    }

    /// 串行阻塞投递一个事件给所有订阅者；无订阅者时直接丢弃。
    ///
    /// 串行投递保证每个订阅者队列中的全局事件顺序一致。队列满时
    /// `tx.send` 挂起——这是背压的传导点；订阅者断开时投递立即报错，
    /// 记录后统一从注册表移除。
    ///
    /// `seal` 之后只有 End 事件仍会被投递，其余输出事件被丢弃——这样
    /// 进程已回收而孙进程仍持有管道写端时，reader 的后续输出不会排在
    /// End 之后造成乱序。
    pub(crate) async fn send(&self, event: ProcessEvent) {
        if self.inner.sealed.load(Ordering::Relaxed) && !matches!(event, ProcessEvent::End(_)) {
            return;
        }
        let subscribers: Vec<Arc<mpsc::Sender<ProcessEvent>>> = {
            let guard = self.inner.subscribers.lock().unwrap();
            guard
                .iter()
                .map(|subscriber| subscriber.tx.clone())
                .collect()
        };
        let mut dead = Vec::new();
        for tx in &subscribers {
            if tx.send(event.clone()).await.is_err() {
                dead.push(tx.clone());
            }
        }
        if !dead.is_empty() {
            let mut guard = self.inner.subscribers.lock().unwrap();
            guard.retain(|subscriber| !dead.iter().any(|gone| Arc::ptr_eq(&subscriber.tx, gone)));
        }
    }

    /// 封住输出：之后 reader 的任何 stdout/stderr/pty 事件都会被丢弃，
    /// 但 End 事件仍可投递。原子置位，无需与正在进行的投递同步——任何
    /// 已经入队的事件都先于调用方后续发出的 End。
    pub(crate) fn seal(&self) {
        self.inner.sealed.store(true, Ordering::Relaxed);
    }

    /// 在 End 事件已投递并写入结束缓存后调用：关闭全部订阅者队列，
    /// 消费端随后收到 `None` 结束流。
    pub(crate) fn close(&self) {
        self.inner.subscribers.lock().unwrap().clear();
    }
}

impl OutputSubscription {
    /// 从订阅队列取下一个事件；队列关闭后返回 `None`。
    pub(crate) fn poll_recv(&mut self, context: &mut Context<'_>) -> Poll<Option<ProcessEvent>> {
        self.rx.poll_recv(context)
    }

    /// 异步接收下一个事件；队列关闭后返回 `None`。
    #[cfg(test)]
    pub(crate) async fn recv(&mut self) -> Option<ProcessEvent> {
        self.rx.recv().await
    }
}
