// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Per-process output bus: one publisher (the pipe/pty pump, plus the
//! supervisor for control events) and one subscription per attached stream
//! (`process.Process/Start`, each `process.Process/Connect`).
//!
//! Policy
//! ------
//! * **Slow is not dead.** A subscriber that is behind applies backpressure:
//!   the pump stops reading the child's pipe, the pipe fills, and the child
//!   blocks in `write`. Nothing is dropped, so a slow consumer still receives
//!   every byte.
//! * **Only a stalled subscriber is evicted.** A subscriber that is *waiting*
//!   and has made no progress for [`DEFAULT_EVICT_AFTER`] is disconnected with
//!   `resource_exhausted`, so a client that stays connected but never reads
//!   cannot pin the pump — the failure mode upstream
//!   [e2b-dev/runtime#3292](https://github.com/e2b-dev/runtime/issues/3292)
//!   reports for a blocking fan-out.
//! * **Terminal events are guaranteed.** Every queue keeps one slot reserved
//!   for its terminal frame through a resident `OwnedPermit`, so `End` is
//!   delivered even when every data slot is full, and the frame order (data
//!   before `End`) is preserved.
//! * **Control events are best-effort.** `DeadlineExceeded` only *hints* that
//!   the process was killed by its deadline; the stream trailer is decided
//!   out-of-band from `EndEvent.killed_by`, so a dropped hint cannot
//!   misreport the stream as a normal exit.

use std::sync::atomic::{AtomicU64, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex, Weak};
use std::time::{Duration, Instant};

use tokio::sync::{mpsc, watch};

use crate::process::engine::PumpEvent;

/// Subscriber-queue capacity: data slots plus the reserved terminal slot.
///
/// One slot holds one read chunk (128 KiB decoded, ~175 KiB framed), and the
/// per-attach memory budget that these slots dominate is ~1 MiB, so a deep
/// queue would cost megabytes per attachment. Depth only buys slack for a
/// client that reads in bursts — the throughput floor is one frame per
/// eviction window either way — and this sandbox's handoff cost makes a *few
/// large* frames much cheaper than many small ones.
pub(crate) const SUBSCRIBER_QUEUE_CAPACITY: usize = 4;
/// Per-connection response queue: data slots plus the reserved terminal slot.
pub(crate) const BODY_QUEUE_CAPACITY: usize = 4;
/// No progress for this long while waiting means the subscriber is stalled.
pub(crate) const DEFAULT_EVICT_AFTER: Duration = Duration::from_secs(300);
/// Attachments allowed per process, including its `Start` subscription.
pub(crate) const MAX_SUBSCRIBERS_PER_PROCESS: usize = 8;
/// Attachments allowed in this envd process, across all processes.
pub(crate) const MAX_SUBSCRIBERS_GLOBAL: usize = 64;

/// The production ceiling, for the probe test in `engine::spawn`.
#[cfg(test)]
pub(crate) fn global_limit_for_tests() -> usize {
    MAX_SUBSCRIBERS_GLOBAL
}

static GLOBAL_SUBSCRIBERS: AtomicUsize = AtomicUsize::new(0);

/// Why a subscription stopped receiving events.
#[derive(Debug, PartialEq, Eq)]
pub enum BusError {
    /// Evicted after making no progress while waiting.
    Evicted,
    /// The publisher is gone and no further events will arrive.
    Closed,
    /// This process (or this envd) already holds as many attachments as allowed.
    TooManySubscribers,
}

/// A bounded queue with one slot permanently reserved for its terminal frame.
///
/// The reservation is a resident `OwnedPermit`: it is held out of
/// `capacity()`, so a data `reserve()` can never take the terminal slot, and
/// the terminal frame can always be queued without blocking. That is what
/// makes "`End` is delivered even when the queue is full" enforceable rather
/// than a convention.
#[derive(Debug)]
pub struct TerminalChannel<T> {
    tx: mpsc::Sender<T>,
    /// Consumed once, when the terminal frame is sent. `Mutex` so that a
    /// `&self` reached through `Arc<Subscriber>` can consume it; the critical
    /// section is synchronous and never held across an await.
    terminal: Mutex<Option<mpsc::OwnedPermit<T>>>,
}

impl<T> TerminalChannel<T> {
    /// Create the channel together with its receiver.
    pub fn new(capacity: usize) -> (Self, mpsc::Receiver<T>) {
        assert!(
            capacity >= 2,
            "a terminal slot needs at least one data slot"
        );
        let (tx, rx) = mpsc::channel(capacity);
        let permit = tx
            .clone()
            .try_reserve_owned()
            .expect("a fresh channel has room for the reserved slot");
        (
            Self {
                tx,
                terminal: Mutex::new(Some(permit)),
            },
            rx,
        )
    }

    /// Wait for a data slot. `reserve` never reports "full" — it waits until a
    /// slot frees up, and only fails once the receiver is gone.
    pub async fn reserve_data(&self) -> Result<mpsc::Permit<'_, T>, BusError> {
        self.tx.reserve().await.map_err(|_| BusError::Closed)
    }

    /// Take a data slot without waiting. `Err` means the queue is full or the
    /// receiver is gone; the caller decides which of the two it is.
    pub fn try_reserve_data(&self) -> Result<mpsc::Permit<'_, T>, BusError> {
        self.tx.try_reserve().map_err(|_| BusError::Closed)
    }

    /// Queue the terminal frame in its reserved slot. Never blocks.
    pub fn send_terminal(&self, value: T) -> bool {
        match lock(&self.terminal).take() {
            Some(permit) => {
                permit.send(value);
                true
            }
            // Only reachable when a terminal frame was already sent; a
            // duplicate is best effort.
            None => self.tx.try_send(value).is_ok(),
        }
    }

    /// Best-effort, non-blocking send, used for control hints.
    pub fn try_send_control(&self, value: T) -> bool {
        self.tx.try_send(value).is_ok()
    }

    /// Resolves once the receiver is gone, i.e. the client disconnected.
    pub async fn closed(&self) {
        self.tx.closed().await
    }
}

#[derive(Debug)]
struct Subscriber {
    id: u64,
    q: TerminalChannel<PumpEvent>,
    /// Publishers currently waiting for a data slot. A data pump publishes for
    /// the *same* subscriber as its sibling (stdout and stderr share a bus), so
    /// the marker below must outlive every waiter, not just the last one to
    /// send: it is raised by the first waiter and lowered by the last one.
    stalled: AtomicUsize,
    /// Set while `stalled` is non-zero. The reaper evicts a subscriber whose
    /// marker is older than `evict_after`; because the marker is set before the
    /// wait, a subscriber that never returns from `reserve_data` is still
    /// visible to it.
    stalled_since: Mutex<Option<Instant>>,
    /// Eviction latch: set by the reaper, awaited by the publisher and by the
    /// connection driving this subscription.
    evicted: watch::Sender<bool>,
}

impl Subscriber {
    /// Record this publisher as waiting for a data slot. The guard clears the
    /// marker when the last waiter leaves — on success, on failure and on
    /// cancellation alike — so no other publisher can erase a stall that is
    /// still in progress, and a dropped wait cannot leave a stale marker behind
    /// for the reaper to evict a healthy subscriber with.
    fn begin_wait(self: &Arc<Self>) -> StallWait<'_> {
        if self.stalled.fetch_add(1, Ordering::AcqRel) == 0 {
            *lock(&self.stalled_since) = Some(Instant::now());
        }
        StallWait { subscriber: self }
    }
}

struct StallWait<'a> {
    subscriber: &'a Subscriber,
}

impl Drop for StallWait<'_> {
    fn drop(&mut self) {
        if self.subscriber.stalled.fetch_sub(1, Ordering::AcqRel) == 1 {
            *lock(&self.subscriber.stalled_since) = None;
        }
    }
}

/// One attachment's view of the bus. Dropping it detaches immediately.
#[derive(Debug)]
pub struct Subscription {
    bus: Weak<OutputBus>,
    id: u64,
    rx: mpsc::Receiver<PumpEvent>,
    evicted: watch::Receiver<bool>,
    /// Terminal-event cache owned by the process table. A subscription that
    /// ends without a terminal event (the bus disappeared first) can still
    /// report the real exit instead of an internal error.
    terminal: Option<Arc<Mutex<Option<PumpEvent>>>>,
    /// The budget this attachment was counted against, if any. A cached
    /// one-shot carries `None`; everything else points at the process-wide
    /// counter (or, in a test, at a private one).
    counter: Option<&'static AtomicUsize>,
}

impl Drop for Subscription {
    fn drop(&mut self) {
        if let Some(bus) = self.bus.upgrade() {
            bus.detach(self.id);
        }
        if let Some(counter) = self.counter {
            counter.fetch_sub(1, Ordering::AcqRel);
        }
    }
}

/// One attachment slot reserved from the process-wide budget.
///
/// `spawn` takes one *before* it creates the child, so an exhausted budget
/// refuses the command instead of leaving a process whose first stream could
/// never be attached. Handing the claim to [`OutputBus::new_claimed`] transfers
/// the release to the subscription that consumes it; dropping it unconsumed
/// returns the slot.
#[derive(Debug)]
pub(crate) struct AttachmentClaim {
    counter: &'static AtomicUsize,
}

impl AttachmentClaim {
    /// Reserve one slot from the production budget.
    pub(crate) fn claim() -> Result<Self, BusError> {
        Self::claim_from(&GLOBAL_SUBSCRIBERS, MAX_SUBSCRIBERS_GLOBAL)
    }

    fn claim_from(global: &'static AtomicUsize, max_global: usize) -> Result<Self, BusError> {
        // Check and increment in one atomic step: `fetch_add` followed by a
        // comparison lets concurrent claims push the counter past the ceiling
        // before either of them notices.
        global
            .fetch_update(Ordering::AcqRel, Ordering::Acquire, |held| {
                (held < max_global).then_some(held + 1)
            })
            .map(|_| Self { counter: global })
            .map_err(|_| BusError::TooManySubscribers)
    }

    /// Hand the counted slot to the subscription that releases it on drop.
    fn into_counter(self) -> &'static AtomicUsize {
        let counter = self.counter;
        // The subscription owns the release from here; forgetting the guard is
        // what stops the same slot from being returned twice.
        std::mem::forget(self);
        counter
    }
}

impl Drop for AttachmentClaim {
    fn drop(&mut self) {
        self.counter.fetch_sub(1, Ordering::AcqRel);
    }
}

/// The per-process output bus handle stored on the process entry.
#[derive(Debug)]
pub struct OutputBus {
    subscribers: Mutex<Vec<Arc<Subscriber>>>,
    next_id: AtomicU64,
    evict_after: Duration,
    max_subscribers: usize,
    /// Budget that attachments of this bus are counted against, and its
    /// ceiling. Production always uses [`GLOBAL_SUBSCRIBERS`] and
    /// [`MAX_SUBSCRIBERS_GLOBAL`]; they are fields because a test has to drive
    /// the global limit without racing every other test in the same binary.
    global: &'static AtomicUsize,
    max_global: usize,
}

impl OutputBus {
    /// Create a bus plus the subscription reserved for its first attachment,
    /// claiming that attachment from the process-wide budget.
    ///
    /// Production goes through [`OutputBus::new_claimed`], which takes the claim
    /// before the child exists; this form claims and builds in one step, which
    /// is what the bus tests drive directly.
    #[cfg(test)]
    pub fn new() -> Result<(Arc<Self>, Subscription), BusError> {
        Self::with_limits(
            SUBSCRIBER_QUEUE_CAPACITY,
            DEFAULT_EVICT_AFTER,
            MAX_SUBSCRIBERS_PER_PROCESS,
        )
    }

    /// Same as [`OutputBus::new`] with explicit limits, so tests can drive
    /// overflow and eviction deterministically.
    #[cfg(test)]
    pub(crate) fn with_limits(
        capacity: usize,
        evict_after: Duration,
        max_subscribers: usize,
    ) -> Result<(Arc<Self>, Subscription), BusError> {
        Self::with_global_limits(
            capacity,
            evict_after,
            max_subscribers,
            &GLOBAL_SUBSCRIBERS,
            MAX_SUBSCRIBERS_GLOBAL,
        )
    }

    /// [`OutputBus::with_limits`] with the global budget injected, so a test can
    /// exhaust it deterministically.
    #[cfg(test)]
    pub(crate) fn with_global_limits(
        capacity: usize,
        evict_after: Duration,
        max_subscribers: usize,
        global: &'static AtomicUsize,
        max_global: usize,
    ) -> Result<(Arc<Self>, Subscription), BusError> {
        let bus = Self::build(evict_after, max_subscribers, global, max_global);
        // Claim *before* attaching. A fresh bus is not exempt from the global
        // budget: its first attachment is exactly the one that used to escape it.
        let claim = AttachmentClaim::claim_from(global, max_global)?;
        let subscription = bus.attach_claimed(capacity, claim);
        bus.spawn_reaper();
        Ok((bus, subscription))
    }

    /// A bus whose first attachment is the slot `claim` already reserved.
    ///
    /// `spawn` reserves the slot before it creates the child, so nothing here
    /// can fail: the budget was checked when the claim was taken.
    pub(crate) fn new_claimed(claim: AttachmentClaim) -> (Arc<Self>, Subscription) {
        let bus = Self::build(
            DEFAULT_EVICT_AFTER,
            MAX_SUBSCRIBERS_PER_PROCESS,
            &GLOBAL_SUBSCRIBERS,
            MAX_SUBSCRIBERS_GLOBAL,
        );
        let subscription = bus.attach_claimed(SUBSCRIBER_QUEUE_CAPACITY, claim);
        bus.spawn_reaper();
        (bus, subscription)
    }

    fn build(
        evict_after: Duration,
        max_subscribers: usize,
        global: &'static AtomicUsize,
        max_global: usize,
    ) -> Arc<Self> {
        Arc::new(Self {
            subscribers: Mutex::new(Vec::new()),
            next_id: AtomicU64::new(1),
            evict_after,
            max_subscribers,
            global,
            max_global,
        })
    }

    /// The first attachment of a bus. It skips the per-process limit -- by the
    /// time it runs the child already exists, so refusing it would strand that
    /// child -- but it never skips the global one: the slot arrives already
    /// counted, as an [`AttachmentClaim`].
    fn attach_claimed(self: &Arc<Self>, capacity: usize, claim: AttachmentClaim) -> Subscription {
        let subscribers = lock(&self.subscribers);
        self.push_subscriber(subscribers, capacity, claim.into_counter())
    }

    fn attach(
        self: &Arc<Self>,
        capacity: usize,
        enforce_per_process_limit: bool,
    ) -> Result<Subscription, BusError> {
        // One critical section for check-and-push, so concurrent `Connect`s
        // cannot both pass the per-process check.
        let subscribers = lock(&self.subscribers);
        if enforce_per_process_limit && subscribers.len() >= self.max_subscribers {
            return Err(BusError::TooManySubscribers);
        }
        // The process-wide budget bounds total per-subscriber queue memory, so
        // it is checked for *every* attachment, first ones included.
        if self.global.fetch_add(1, Ordering::AcqRel) >= self.max_global {
            self.global.fetch_sub(1, Ordering::AcqRel);
            return Err(BusError::TooManySubscribers);
        }
        Ok(self.push_subscriber(subscribers, capacity, self.global))
    }

    /// Append a subscriber that has already been counted against `counter`.
    fn push_subscriber(
        self: &Arc<Self>,
        mut subscribers: std::sync::MutexGuard<'_, Vec<Arc<Subscriber>>>,
        capacity: usize,
        counter: &'static AtomicUsize,
    ) -> Subscription {
        let (q, rx) = TerminalChannel::new(capacity);
        let (evicted_tx, evicted_rx) = watch::channel(false);
        let id = self.next_id.fetch_add(1, Ordering::Relaxed);
        subscribers.push(Arc::new(Subscriber {
            id,
            q,
            stalled: AtomicUsize::new(0),
            stalled_since: Mutex::new(None),
            evicted: evicted_tx,
        }));
        drop(subscribers);
        Subscription {
            bus: Arc::downgrade(self),
            id,
            rx,
            evicted: evicted_rx,
            terminal: None,
            counter: Some(counter),
        }
    }

    /// Attach another subscription (a later `Connect`).
    pub fn subscribe(self: &Arc<Self>) -> Result<Subscription, BusError> {
        self.attach(SUBSCRIBER_QUEUE_CAPACITY, true)
    }

    /// A subscription that yields exactly one cached event and then closes.
    /// Used when a `Connect` arrives after the terminal event was published but
    /// before the process entry was removed (removal happens at child reap).
    pub fn subscription_from_event(event: PumpEvent) -> Subscription {
        let (q, rx) = TerminalChannel::new(2);
        let _ = q.send_terminal(event);
        let (evicted_tx, evicted_rx) = watch::channel(false);
        drop(evicted_tx);
        Subscription {
            bus: Weak::new(),
            id: 0,
            rx,
            evicted: evicted_rx,
            terminal: None,
            counter: None,
        }
    }

    /// How many attachments are currently attached.
    pub fn subscriber_count(&self) -> usize {
        lock(&self.subscribers).len()
    }

    /// Publish a data event, waiting for room in every live subscriber. The
    /// wait is interruptible by eviction, and a subscriber that is behind is
    /// what backpressures the child.
    pub async fn publish_data(&self, event: PumpEvent) {
        for subscriber in self.snapshot() {
            // Fast path: room is available, so this publish neither waits nor
            // can be evicted mid-flight. It skips the eviction receiver and the
            // select — the per-frame cost the sandbox's handoff overhead makes
            // visible. It must not touch the stall marker either: stdout and
            // stderr publish to the same subscriber, so another publisher can be
            // entering `begin_wait` right now, and a `take()` here would erase a
            // wait that is about to start and leave it invisible to the reaper
            // forever. The marker is maintained by `begin_wait`/`StallWait`
            // alone, which keeps "marker is set" and "somebody is waiting" in
            // step.
            if let Ok(permit) = subscriber.q.try_reserve_data() {
                permit.send(event.clone());
                continue;
            }
            let mut evicted = subscriber.evicted.subscribe();
            if *evicted.borrow() {
                continue;
            }
            // Mark the stall *before* waiting. A genuinely stuck subscriber
            // never returns from `reserve_data`, so this marker is the only
            // thing the reaper can act on.
            let waiting = subscriber.begin_wait();
            let _sent = tokio::select! {
                biased;
                _ = evicted.changed() => false,
                permit = subscriber.q.reserve_data() => match permit {
                    Ok(permit) => {
                        permit.send(event.clone());
                        true
                    }
                    Err(_) => false,
                },
            };
            drop(waiting);
        }
    }

    /// Publish the terminal event. Guaranteed and non-blocking: every queue
    /// holds a slot reserved for it.
    pub fn publish_terminal(&self, event: PumpEvent) -> bool {
        let subscribers = self.snapshot();
        if subscribers.is_empty() {
            return false;
        }
        for subscriber in &subscribers {
            if !subscriber.q.send_terminal(event.clone()) {
                tracing::warn!("process output bus: terminal frame dropped for a subscriber");
            }
        }
        true
    }

    /// Publish a best-effort control event (`DeadlineExceeded`). Never blocks,
    /// never evicts, and may be dropped when a subscriber is behind.
    pub fn publish_control(&self, event: PumpEvent) -> bool {
        let subscribers = self.snapshot();
        if subscribers.is_empty() {
            return false;
        }
        for subscriber in &subscribers {
            subscriber.q.try_send_control(event.clone());
        }
        true
    }

    /// Disconnect every subscriber whose wait has made no progress for
    /// `evict_after`. Called by the per-bus reaper task.
    fn evict_stalled(&self) {
        let now = Instant::now();
        let stalled: Vec<u64> = self
            .snapshot()
            .iter()
            .filter(|subscriber| {
                // A waiter is required, not just a marker: the marker is
                // dropped a moment after the count reaches zero, and evicting
                // in that window would disconnect a subscriber whose publisher
                // has just made progress.
                subscriber.stalled.load(Ordering::Acquire) > 0
                    && lock(&subscriber.stalled_since)
                        .is_some_and(|since| now.duration_since(since) >= self.evict_after)
            })
            .map(|subscriber| subscriber.id)
            .collect();
        for id in stalled {
            if let Some(subscriber) = self.take(id) {
                tracing::info!(
                    "process output bus: evicting subscriber {id} after {:?} without progress",
                    self.evict_after
                );
                // Wakes the publisher's wait and the connection's driver. The
                // connection reports `resource_exhausted` on its own terminal
                // slot, because it owns the encoding. Frames still queued for
                // this subscriber are abandoned with it (the client keeps
                // whatever already reached its connection queue), which is the
                // point: the subscriber is the reason the queue filled up.
                let _ = subscriber.evicted.send(true);
            }
        }
    }

    fn spawn_reaper(self: &Arc<Self>) {
        // Buses are built inside the runtime in production; sync unit tests
        // construct them without one, and there is nothing to reap there.
        let Ok(handle) = tokio::runtime::Handle::try_current() else {
            return;
        };
        let bus = Arc::downgrade(self);
        let tick = (self.evict_after / 10).clamp(Duration::from_millis(5), Duration::from_secs(1));
        handle.spawn(async move {
            let mut interval = tokio::time::interval(tick);
            interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
            loop {
                interval.tick().await;
                match bus.upgrade() {
                    Some(bus) => bus.evict_stalled(),
                    None => return,
                }
            }
        });
    }

    fn snapshot(&self) -> Vec<Arc<Subscriber>> {
        lock(&self.subscribers).clone()
    }

    fn take(&self, id: u64) -> Option<Arc<Subscriber>> {
        let mut guard = lock(&self.subscribers);
        let index = guard.iter().position(|subscriber| subscriber.id == id)?;
        Some(guard.remove(index))
    }

    fn detach(&self, id: u64) {
        lock(&self.subscribers).retain(|subscriber| subscriber.id != id);
    }
}

impl Subscription {
    /// Whether the eviction latch has been set.
    pub fn is_evicted(&self) -> bool {
        *self.evicted.borrow()
    }

    /// The eviction latch, for a driver that must also abandon a blocked send.
    pub fn eviction(&self) -> watch::Receiver<bool> {
        self.evicted.clone()
    }

    /// Attach the process table's terminal cache to this subscription.
    pub fn watch_terminal_cache(&mut self, cache: Arc<Mutex<Option<PumpEvent>>>) {
        self.terminal = Some(cache);
    }

    /// The cached terminal event, if the process already published one.
    pub fn terminal_event(&self) -> Option<PumpEvent> {
        self.terminal.as_ref().and_then(|cache| lock(cache).clone())
    }

    /// Receive the next event. Cancel-safe: dropping the future loses nothing.
    pub async fn recv(&mut self) -> Result<PumpEvent, BusError> {
        match self.rx.recv().await {
            Some(event) => Ok(event),
            None if self.is_evicted() => Err(BusError::Evicted),
            None => Err(BusError::Closed),
        }
    }
}

/// Create the per-connection response queue (data slots plus a terminal slot).
pub fn body_channel() -> (TerminalChannel<bytes::Bytes>, mpsc::Receiver<bytes::Bytes>) {
    TerminalChannel::new(BODY_QUEUE_CAPACITY)
}

fn lock<T>(mutex: &Mutex<T>) -> std::sync::MutexGuard<'_, T> {
    mutex
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn event() -> PumpEvent {
        PumpEvent::DeadlineExceeded
    }

    /// Fast limits so stall and eviction paths run in milliseconds.
    fn bus_with(capacity: usize, evict_after: Duration) -> (Arc<OutputBus>, Subscription) {
        OutputBus::with_limits(capacity, evict_after, MAX_SUBSCRIBERS_PER_PROCESS)
            .expect("a fresh bus")
    }

    #[tokio::test]
    async fn publish_data_reaches_every_subscriber() {
        let (bus, mut first) = OutputBus::new().expect("a fresh bus");
        let mut second = bus.subscribe().unwrap();
        assert_eq!(bus.subscriber_count(), 2);
        bus.publish_data(event()).await;
        assert!(first.recv().await.is_ok());
        assert!(second.recv().await.is_ok());
    }

    #[tokio::test]
    async fn dropping_a_subscription_detaches_it_immediately() {
        let (bus, first) = OutputBus::new().expect("a fresh bus");
        let second = bus.subscribe().unwrap();
        assert_eq!(bus.subscriber_count(), 2);
        drop(second);
        assert_eq!(bus.subscriber_count(), 1);
        drop(first);
        assert_eq!(bus.subscriber_count(), 0);
    }

    #[tokio::test]
    async fn closed_bus_reports_closed() {
        let (bus, mut subscription) = OutputBus::new().expect("a fresh bus");
        drop(bus);
        assert!(matches!(subscription.recv().await, Err(BusError::Closed)));
    }

    #[tokio::test]
    async fn cached_event_subscription_yields_once_then_closes() {
        let mut subscription = OutputBus::subscription_from_event(event());
        assert!(subscription.recv().await.is_ok());
        assert!(matches!(subscription.recv().await, Err(BusError::Closed)));
    }

    #[tokio::test]
    async fn data_never_consumes_the_reserved_terminal_slot() {
        let (bus, mut subscription) = bus_with(2, Duration::from_secs(60));
        // Two data events: one fills the only data slot, the other waits.
        let sender = tokio::spawn({
            let bus = Arc::clone(&bus);
            async move {
                bus.publish_data(event()).await;
                bus.publish_data(event()).await;
            }
        });
        // The terminal frame fits anyway: its slot was never a data slot.
        assert!(bus.publish_terminal(event()));
        assert!(subscription.recv().await.is_ok(), "data event");
        assert!(subscription.recv().await.is_ok(), "terminal event");
        sender.abort();
    }

    #[tokio::test]
    async fn slow_subscriber_is_backpressured_not_dropped() {
        let (bus, mut subscription) = bus_with(2, Duration::from_secs(60));
        let publisher = tokio::spawn({
            let bus = Arc::clone(&bus);
            async move {
                for _ in 0..5 {
                    bus.publish_data(event()).await;
                }
            }
        });
        // The publisher can only get one event ahead of this reader.
        tokio::time::sleep(Duration::from_millis(20)).await;
        assert!(!publisher.is_finished(), "publisher must wait for room");
        for _ in 0..5 {
            assert!(subscription.recv().await.is_ok());
        }
        tokio::time::timeout(Duration::from_secs(1), publisher)
            .await
            .expect("publisher resumed once the subscriber drained")
            .unwrap();
    }

    #[tokio::test]
    async fn stalled_but_connected_subscriber_is_evicted() {
        // Capacity 2 with one reserved ⇒ one data slot. The subscriber never
        // reads, so the second publish waits and must be broken by the reaper.
        let (bus, subscription) = bus_with(2, Duration::from_millis(60));
        let publisher = tokio::spawn({
            let bus = Arc::clone(&bus);
            async move {
                bus.publish_data(event()).await;
                bus.publish_data(event()).await;
                "published"
            }
        });
        let outcome = tokio::time::timeout(Duration::from_secs(2), publisher)
            .await
            .expect("a stalled subscriber must not pin the publisher forever")
            .unwrap();
        assert_eq!(outcome, "published");
        assert_eq!(bus.subscriber_count(), 0, "the stalled subscriber is gone");
        assert!(subscription.is_evicted());
    }

    /// A publish that did not wait must leave the marker alone. This is the
    /// state a concurrent publisher leaves behind while it is inside
    /// `begin_wait`, and clearing it here used to make that wait invisible to
    /// the reaper for good (the subscriber could then never be evicted).
    #[tokio::test]
    async fn a_publish_that_did_not_wait_leaves_the_stall_marker_alone() {
        let (bus, _sub) = bus_with(4, Duration::from_secs(60));
        let subscriber = bus.snapshot()[0].clone();
        *lock(&subscriber.stalled_since) = Some(Instant::now() - Duration::from_secs(1));

        bus.publish_data(event()).await; // uncontended: takes the fast path

        assert!(
            lock(&subscriber.stalled_since).is_some(),
            "a publish that never waited must not clear a marker it did not set"
        );
    }

    /// The reaper needs a waiter, not just a marker: between the last waiter's
    /// decrement and its clear the marker is briefly set with nobody waiting.
    #[tokio::test]
    async fn the_reaper_ignores_a_marker_with_no_waiter() {
        let (bus, _sub) = bus_with(4, Duration::from_millis(1));
        let subscriber = bus.snapshot()[0].clone();
        *lock(&subscriber.stalled_since) = Some(Instant::now() - Duration::from_secs(60));

        bus.evict_stalled();

        assert_eq!(
            bus.subscriber_count(),
            1,
            "no waiter means no eviction, whatever the marker says"
        );
        assert!(lock(&subscriber.stalled_since).is_some());
    }

    /// The invariant the fast path relies on: a wait that completes clears both
    /// the count and the marker, so no cleanup is needed on the publish path.
    #[tokio::test]
    async fn a_completed_wait_clears_the_marker_and_the_count() {
        let (bus, mut sub) = bus_with(2, Duration::from_secs(60));
        bus.publish_data(event()).await; // fills the only data slot
        let mut blocked = Box::pin(bus.publish_data(event()));
        assert!(futures::poll!(blocked.as_mut()).is_pending());
        let subscriber = bus.snapshot()[0].clone();
        assert_eq!(subscriber.stalled.load(Ordering::Acquire), 1);
        assert!(lock(&subscriber.stalled_since).is_some());

        let _ = sub.recv().await; // frees the slot for the waiting publisher
        blocked.await;

        assert_eq!(subscriber.stalled.load(Ordering::Acquire), 0);
        assert!(lock(&subscriber.stalled_since).is_none());
    }

    /// An uncontended publish takes the fast path, which must leave the stall
    /// bookkeeping untouched (no marker for the reaper to act on).
    #[tokio::test]
    async fn an_uncontended_publish_leaves_no_stall_marker() {
        let (bus, _sub) = bus_with(4, Duration::from_secs(60));
        bus.publish_data(event()).await;
        let subscriber = bus.snapshot()[0].clone();
        assert_eq!(subscriber.stalled.load(Ordering::Acquire), 0);
        assert!(lock(&subscriber.stalled_since).is_none());
    }

    /// The process-wide attachment budget had no test: a regression here either
    /// leaks the budget (attachments refused forever) or fails to enforce it
    /// (unbounded memory). The budget is injected so the test does not race
    /// every other test's subscriptions.
    #[tokio::test]
    async fn the_global_attachment_budget_is_enforced_and_released() {
        static GLOBAL: AtomicUsize = AtomicUsize::new(0);
        let (bus, first) = OutputBus::with_global_limits(2, Duration::from_secs(60), 8, &GLOBAL, 2)
            .expect("a fresh bus");
        assert_eq!(GLOBAL.load(Ordering::Acquire), 1, "the first attach counts");
        let second = bus.subscribe().unwrap();
        assert_eq!(GLOBAL.load(Ordering::Acquire), 2);
        assert!(matches!(bus.subscribe(), Err(BusError::TooManySubscribers)));
        assert_eq!(GLOBAL.load(Ordering::Acquire), 2, "a refusal leaks nothing");

        drop(second);
        assert_eq!(GLOBAL.load(Ordering::Acquire), 1, "detach returns the slot");
        let third = bus.subscribe().unwrap();
        assert_eq!(GLOBAL.load(Ordering::Acquire), 2);

        // The cached one-shot a late `Connect` gets is not an attachment.
        let cached = OutputBus::subscription_from_event(PumpEvent::DeadlineExceeded);
        assert_eq!(GLOBAL.load(Ordering::Acquire), 2);
        drop(cached);
        assert_eq!(GLOBAL.load(Ordering::Acquire), 2, "one-shots are uncounted");

        drop((first, third));
        assert_eq!(GLOBAL.load(Ordering::Acquire), 0);
    }

    /// Two processes share one budget, and the budget is a ceiling for *every*
    /// attachment -- including the first one of a fresh bus. `spawn` claims the
    /// slot before the child exists, so a new bus at an exhausted budget is
    /// refused instead of silently becoming attachment 65.
    #[tokio::test]
    async fn the_global_budget_bounds_the_first_attachment_too() {
        static GLOBAL: AtomicUsize = AtomicUsize::new(0);
        let (bus_a, first_a) =
            OutputBus::with_global_limits(2, Duration::from_secs(60), 4, &GLOBAL, 3)
                .expect("a fresh bus");
        let (bus_b, first_b) =
            OutputBus::with_global_limits(2, Duration::from_secs(60), 4, &GLOBAL, 3)
                .expect("a fresh bus");
        assert_eq!(GLOBAL.load(Ordering::Acquire), 2);
        let second_a = bus_a.subscribe().unwrap();
        assert_eq!(GLOBAL.load(Ordering::Acquire), 3);
        assert!(matches!(
            bus_b.subscribe(),
            Err(BusError::TooManySubscribers)
        ));

        // Exhausted budget, new process: the first attachment is refused, and
        // the refusal leaves the counter exactly at the ceiling.
        assert!(matches!(
            OutputBus::with_global_limits(2, Duration::from_secs(60), 4, &GLOBAL, 3),
            Err(BusError::TooManySubscribers)
        ));
        assert_eq!(
            GLOBAL.load(Ordering::Acquire),
            3,
            "a refused bus may not consume or exceed the budget"
        );
        assert!(matches!(
            AttachmentClaim::claim_from(&GLOBAL, 3),
            Err(BusError::TooManySubscribers)
        ));
        assert_eq!(GLOBAL.load(Ordering::Acquire), 3);

        drop(second_a);
        assert_eq!(
            GLOBAL.load(Ordering::Acquire),
            2,
            "bus A's slot is reusable"
        );
        let (_bus_c, first_c) =
            OutputBus::with_global_limits(2, Duration::from_secs(60), 4, &GLOBAL, 3)
                .expect("the freed slot admits a fresh bus");
        assert_eq!(GLOBAL.load(Ordering::Acquire), 3);
        assert!(matches!(
            bus_b.subscribe(),
            Err(BusError::TooManySubscribers)
        ));
        assert_eq!(GLOBAL.load(Ordering::Acquire), 3, "the ceiling still holds");
        drop((first_a, first_b, first_c));
        assert_eq!(GLOBAL.load(Ordering::Acquire), 0);
    }

    /// The counter is a hard ceiling no matter which path reaches for a slot:
    /// fresh buses, later `Connect`s and bare claims all stop at `max_global`
    /// and none of them leaks a slot on the way out.
    #[tokio::test]
    async fn the_global_counter_never_exceeds_its_ceiling() {
        static GLOBAL: AtomicUsize = AtomicUsize::new(0);
        let mut held = Vec::new();
        for _ in 0..5 {
            match OutputBus::with_global_limits(2, Duration::from_secs(60), 4, &GLOBAL, 4) {
                Ok(pair) => held.push(pair),
                Err(_) => break,
            }
            assert!(
                GLOBAL.load(Ordering::Acquire) <= 4,
                "a fresh bus pushed the counter past its ceiling"
            );
        }
        assert_eq!(GLOBAL.load(Ordering::Acquire), 4, "the budget fills up");
        assert!(matches!(
            OutputBus::with_global_limits(2, Duration::from_secs(60), 4, &GLOBAL, 4),
            Err(BusError::TooManySubscribers)
        ));
        assert_eq!(
            GLOBAL.load(Ordering::Acquire),
            4,
            "the refusal leaks nothing"
        );
        for (bus, _first) in &held {
            assert!(matches!(bus.subscribe(), Err(BusError::TooManySubscribers)));
            assert!(GLOBAL.load(Ordering::Acquire) <= 4);
        }
        drop(held);
        assert_eq!(GLOBAL.load(Ordering::Acquire), 0);
    }

    /// `spawn` claims before it creates the child, so the claim has to release
    /// the slot on every early return: a failed command must not hold a budget
    /// slot for a process that never existed.
    #[tokio::test]
    async fn a_dropped_claim_returns_its_slot() {
        static GLOBAL: AtomicUsize = AtomicUsize::new(0);
        let claim = AttachmentClaim::claim_from(&GLOBAL, 1).expect("the budget is free");
        assert_eq!(GLOBAL.load(Ordering::Acquire), 1);
        assert!(matches!(
            AttachmentClaim::claim_from(&GLOBAL, 1),
            Err(BusError::TooManySubscribers)
        ));
        assert_eq!(
            GLOBAL.load(Ordering::Acquire),
            1,
            "a refused claim leaks nothing"
        );
        drop(claim);
        assert_eq!(GLOBAL.load(Ordering::Acquire), 0);

        // The released slot is immediately usable, and handing a claim to a bus
        // transfers the release to its first subscription.
        let claim = AttachmentClaim::claim_from(&GLOBAL, 1).expect("the slot came back");
        let (bus, first) = OutputBus::new_claimed(claim);
        assert_eq!(GLOBAL.load(Ordering::Acquire), 1);
        drop(first);
        drop(bus);
        assert_eq!(GLOBAL.load(Ordering::Acquire), 0);
    }

    /// A dropped wait must not leave a marker behind: the reaper would evict a
    /// subscriber that is reading normally, `evict_after` later.
    #[tokio::test]
    async fn cancelling_a_wait_clears_the_stall_marker() {
        let (bus, _sub) = bus_with(2, Duration::from_secs(60));
        bus.publish_data(event()).await; // fills the only data slot
        let mut blocked = Box::pin(bus.publish_data(event()));
        assert!(futures::poll!(blocked.as_mut()).is_pending());
        assert!(
            lock(&bus.snapshot()[0].stalled_since).is_some(),
            "a waiting publisher must be visible to the reaper"
        );
        drop(blocked);
        assert!(
            lock(&bus.snapshot()[0].stalled_since).is_none(),
            "cancelling the wait clears the marker"
        );
    }

    #[tokio::test]
    async fn eviction_does_not_touch_an_idle_subscriber() {
        // Nothing was ever published, so nothing is waiting: passing the clock
        // must not evict a subscriber that is simply idle.
        let (bus, subscription) = bus_with(2, Duration::from_millis(30));
        tokio::time::sleep(Duration::from_millis(120)).await;
        assert_eq!(bus.subscriber_count(), 1);
        assert!(!subscription.is_evicted());
    }

    #[tokio::test]
    async fn eviction_is_per_subscriber() {
        let (bus, slow) = bus_with(2, Duration::from_millis(60));
        let mut fast = bus.subscribe().unwrap();
        let publisher = tokio::spawn({
            let bus = Arc::clone(&bus);
            async move {
                for _ in 0..8 {
                    bus.publish_data(event()).await;
                }
            }
        });
        // Only the fast subscriber drains; the slow one stalls and is evicted.
        for _ in 0..8 {
            if fast.recv().await.is_err() {
                break;
            }
        }
        tokio::time::timeout(Duration::from_secs(2), publisher)
            .await
            .expect("publisher resumes after evicting only the stalled subscriber")
            .unwrap();
        assert!(slow.is_evicted());
        assert!(!fast.is_evicted());
    }

    #[tokio::test]
    async fn publishing_without_subscribers_is_a_no_op() {
        let (bus, first) = OutputBus::new().expect("a fresh bus");
        drop(first);
        bus.publish_data(event()).await;
        assert!(!bus.publish_terminal(event()));
        assert!(!bus.publish_control(event()));
    }

    #[tokio::test]
    async fn subscription_limit_is_enforced_per_process() {
        let (bus, _first) =
            OutputBus::with_limits(SUBSCRIBER_QUEUE_CAPACITY, Duration::from_secs(60), 2)
                .expect("a fresh bus");
        let _second = bus.subscribe().unwrap();
        assert_eq!(bus.subscribe().unwrap_err(), BusError::TooManySubscribers);
    }

    #[tokio::test]
    async fn a_cancelled_reserve_does_not_leak_a_slot() {
        // One data slot: fill it, start (and cancel) a waiter, then confirm the
        // freed slot is usable again. A leaked permit would hang the last call.
        let (q, mut body) = TerminalChannel::<u8>::new(2);
        let permit = q.reserve_data().await.unwrap();
        permit.send(1);
        {
            let waiter = q.reserve_data();
            tokio::pin!(waiter);
            assert!(
                futures::poll!(&mut waiter).is_pending(),
                "the queue must be full before the wait"
            );
            // Dropping the future cancels the wait.
        }
        assert_eq!(body.recv().await, Some(1));
        let permit = tokio::time::timeout(std::time::Duration::from_secs(1), q.reserve_data())
            .await
            .expect("a cancelled reserve leaked its slot");
        permit.unwrap().send(2);
        assert_eq!(body.recv().await, Some(2));
    }

    #[tokio::test]
    async fn terminal_frame_always_fits() {
        let (q, mut body) = body_channel();
        for _ in 0..(BODY_QUEUE_CAPACITY - 1) {
            let permit = q.reserve_data().await.unwrap();
            permit.send(bytes::Bytes::from_static(b"data"));
        }
        assert!(q.send_terminal(bytes::Bytes::from_static(b"end")));
        for _ in 0..BODY_QUEUE_CAPACITY {
            assert!(body.try_recv().is_ok());
        }
        assert!(body.try_recv().is_err());
    }

    // The global counter is process-wide, so an absolute assertion would race
    // with the rest of the suite. Its accounting is exercised implicitly: a
    // missed decrement would exhaust the global limit and fail unrelated tests.
}
