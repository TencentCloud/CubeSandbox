// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause

//! One bounded BLKDISCARD worker per virtio queue.
//!
//! A block-device discard is a synchronous ioctl, so running it on the queue
//! thread would stall that thread (and pause/reset) for the duration. Instead
//! the backend hands each discarded range to this worker, which owns a
//! duplicated fd and runs on its own thread; the queue thread keeps draining
//! completions from the same shared state. This only keeps the virtio queue
//! thread out of the synchronous ioctl: the backing device may still serialize
//! or block other I/O, and an ioctl already in flight cannot be cancelled.
//! Reset/drop must therefore join the worker for fd and shared-state safety.
//!
//! The total capacity counts pending, executing, and completed-but-undrained
//! tasks, so the queue stays bounded even if the consumer stops calling
//! `drain()`. Every new submission beyond that capacity is rejected before it
//! is enqueued.

use crate::async_io::block_discard;
use crate::SECTOR_SIZE;
use std::collections::VecDeque;
use std::io;
use std::os::fd::{AsRawFd, OwnedFd, RawFd};
use std::sync::{Arc, Condvar, Mutex};
use vmm_sys_util::eventfd::EventFd;

struct DiscardTask {
    offset: u64,
    len: u64,
    user_data: u64,
}

struct State {
    pending: VecDeque<DiscardTask>,
    completions: VecDeque<(u64, i32)>,
    outstanding: usize,
    closed: bool,
}

struct Shared {
    state: Mutex<State>,
    condvar: Condvar,
    capacity: usize,
    max_sectors: u32,
    notifier: Arc<EventFd>,
}

/// Submitting half of a discard worker, held by the backend.
///
/// Dropping it closes the worker: queued-but-unstarted tasks are skipped and
/// a running ioctl is allowed to finish before the worker thread returns.
pub(crate) struct DiscardSender {
    shared: Arc<Shared>,
}

/// Owned runner half of a discard worker. It owns its duplicated fd, so it may
/// outlive the backend and every other thread that only borrowed the original
/// descriptor.
pub(crate) struct DiscardWorker {
    shared: Arc<Shared>,
    fd: OwnedFd,
}

impl DiscardSender {
    pub(crate) fn new(capacity: usize, max_sectors: u32, notifier: Arc<EventFd>) -> Self {
        DiscardSender {
            shared: Arc::new(Shared {
                state: Mutex::new(State {
                    pending: VecDeque::new(),
                    completions: VecDeque::new(),
                    outstanding: 0,
                    closed: false,
                }),
                condvar: Condvar::new(),
                capacity,
                max_sectors,
                notifier,
            }),
        }
    }

    /// Build the owned runner for this sender. The caller hands the returned
    /// worker to a dedicated thread and drops this sender to stop it.
    pub(crate) fn worker(&self, fd: OwnedFd) -> DiscardWorker {
        DiscardWorker {
            shared: Arc::clone(&self.shared),
            fd,
        }
    }

    /// Enqueue one discard. Fails with `EAGAIN` when the queue is stopped or
    /// full, and with `EINVAL` when the range exceeds the backend maximum; in
    /// every failure case nothing has been published.
    pub(crate) fn submit(&self, offset: u64, len: u64, user_data: u64) -> io::Result<()> {
        if len == 0 || len > u64::from(self.shared.max_sectors) * SECTOR_SIZE {
            return Err(io::Error::from_raw_os_error(libc::EINVAL));
        }

        let mut state = self.shared.state.lock().unwrap();
        if state.closed || state.outstanding >= self.shared.capacity {
            return Err(io::Error::from_raw_os_error(libc::EAGAIN));
        }
        state.pending.push_back(DiscardTask {
            offset,
            len,
            user_data,
        });
        state.outstanding += 1;
        drop(state);
        self.shared.condvar.notify_one();
        Ok(())
    }

    /// Take every completed discard, releasing their capacity.
    pub(crate) fn drain(&self) -> Vec<(u64, i32)> {
        let mut state = self.shared.state.lock().unwrap();
        let drained: Vec<(u64, i32)> = state.completions.drain(..).collect();
        state.outstanding -= drained.len();
        drained
    }
}

impl Drop for DiscardSender {
    fn drop(&mut self) {
        let mut state = self.shared.state.lock().unwrap();
        state.closed = true;
        // Anything still queued was never started; only an in-flight ioctl is
        // allowed to finish.
        state.pending.clear();
        drop(state);
        self.shared.condvar.notify_all();
    }
}

impl DiscardWorker {
    /// Run until the sender is dropped. An error is fatal for the worker
    /// thread: the caller surfaces it through the thread exit path.
    pub(crate) fn run(self) -> io::Result<()> {
        self.run_with(block_discard)
    }

    pub(crate) fn run_with<F>(self, discard: F) -> io::Result<()>
    where
        F: Fn(RawFd, u64, u64) -> io::Result<()>,
    {
        let fd = self.fd.as_raw_fd();
        loop {
            let task = {
                let mut state = self.shared.state.lock().unwrap();
                loop {
                    if state.closed {
                        return Ok(());
                    }
                    if let Some(task) = state.pending.pop_front() {
                        break task;
                    }
                    state = self.shared.condvar.wait(state).unwrap();
                }
            };

            let completion = match discard(fd, task.offset, task.len) {
                Ok(()) => 0,
                Err(error) => -error.raw_os_error().unwrap_or(libc::EIO),
            };

            // Publish before notifying: the notifier is the consumer's only
            // wake-up, so the completion must already be visible when it fires.
            let mut state = self.shared.state.lock().unwrap();
            state.completions.push_back((task.user_data, completion));
            let notified = self.shared.notifier.write(1);
            drop(state);
            match notified {
                // A saturated nonblocking eventfd is already readable, so the
                // retained completion is still drained by the queue.
                Err(error) if error.kind() == io::ErrorKind::WouldBlock => {}
                Err(error) => return Err(error),
                Ok(_) => {}
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::DiscardSender;
    use crate::async_io::block_discard;
    use crate::SECTOR_SIZE;
    use std::io;
    use std::os::fd::{AsRawFd, OwnedFd};
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::sync::{mpsc, Arc};
    use std::thread;
    use std::time::{Duration, Instant};
    use vmm_sys_util::eventfd::EventFd;
    use vmm_sys_util::tempfile::TempFile;

    fn new_queue(capacity: usize, max_sectors: u32) -> (DiscardSender, Arc<EventFd>) {
        let notifier = Arc::new(EventFd::new(libc::EFD_NONBLOCK).unwrap());
        let sender = DiscardSender::new(capacity, max_sectors, notifier.clone());
        (sender, notifier)
    }

    fn owned_fd() -> OwnedFd {
        let temp = TempFile::new().unwrap();
        temp.as_file().try_clone().unwrap().into()
    }

    fn wait_until(what: &str, mut ready: impl FnMut() -> bool) {
        let deadline = Instant::now() + Duration::from_secs(5);
        while !ready() {
            assert!(Instant::now() < deadline, "timed out waiting for {what}");
            thread::sleep(Duration::from_millis(1));
        }
    }

    fn outstanding(sender: &DiscardSender) -> usize {
        sender.shared.state.lock().unwrap().outstanding
    }

    fn pending(sender: &DiscardSender) -> usize {
        sender.shared.state.lock().unwrap().pending.len()
    }

    fn completions(sender: &DiscardSender) -> usize {
        sender.shared.state.lock().unwrap().completions.len()
    }

    #[test]
    fn submit_does_not_execute_inline() {
        let (sender, notifier) = new_queue(4, 16);
        let worker = sender.worker(owned_fd());

        sender.submit(0, 8 * SECTOR_SIZE, 1).unwrap();

        // Nothing may run before the worker thread is started: the task is
        // queued, no completion exists and the notifier was never armed.
        assert_eq!(pending(&sender), 1);
        assert_eq!(outstanding(&sender), 1);
        assert_eq!(completions(&sender), 0);
        assert_eq!(
            notifier.read().unwrap_err().kind(),
            io::ErrorKind::WouldBlock
        );

        let calls = Arc::new(AtomicUsize::new(0));
        let worker_calls = Arc::clone(&calls);
        let handle = thread::spawn(move || {
            worker.run_with(move |_, _, _| {
                worker_calls.fetch_add(1, Ordering::SeqCst);
                Ok(())
            })
        });
        wait_until("the queued discard to complete", || {
            completions(&sender) == 1
        });
        drop(sender);
        handle.join().unwrap().unwrap();
        assert_eq!(calls.load(Ordering::SeqCst), 1);
    }

    #[test]
    fn saturated_capacity_counts_completed_undrained_jobs() {
        let (sender, _notifier) = new_queue(2, 16);
        let worker = sender.worker(owned_fd());
        let handle = thread::spawn(move || worker.run_with(|_, _, _| Ok(())));

        sender.submit(0, SECTOR_SIZE, 1).unwrap();
        sender.submit(0, SECTOR_SIZE, 2).unwrap();
        wait_until("both discards to complete", || completions(&sender) == 2);

        // Both tasks are done but undrained, so capacity is still consumed.
        assert_eq!(outstanding(&sender), 2);
        let rejected = sender.submit(0, SECTOR_SIZE, 3).unwrap_err();
        assert_eq!(rejected.raw_os_error(), Some(libc::EAGAIN));
        assert_eq!(pending(&sender), 0);
        assert_eq!(outstanding(&sender), 2);

        assert_eq!(sender.drain().len(), 2);
        assert_eq!(outstanding(&sender), 0);

        sender.submit(0, SECTOR_SIZE, 4).unwrap();
        wait_until("the replacement discard to complete", || {
            completions(&sender) == 1
        });
        drop(sender);
        handle.join().unwrap().unwrap();
    }

    #[test]
    fn rejection_happens_before_enqueue() {
        let (sender, _notifier) = new_queue(1, 16);
        sender.submit(0, SECTOR_SIZE, 1).unwrap();
        let rejected = sender.submit(0, SECTOR_SIZE, 2).unwrap_err();
        assert_eq!(rejected.raw_os_error(), Some(libc::EAGAIN));
        assert_eq!(pending(&sender), 1);
        assert_eq!(outstanding(&sender), 1);
        assert_eq!(completions(&sender), 0);
    }

    #[test]
    fn oversize_discard_is_rejected_before_enqueue() {
        let (sender, _notifier) = new_queue(4, 2);
        let rejected = sender.submit(0, 3 * SECTOR_SIZE, 9).unwrap_err();
        assert_eq!(rejected.raw_os_error(), Some(libc::EINVAL));
        assert_eq!(pending(&sender), 0);
        assert_eq!(outstanding(&sender), 0);
    }

    #[test]
    fn stop_skips_queued_but_unstarted_work() {
        let (sender, _notifier) = new_queue(4, 16);
        let worker = sender.worker(owned_fd());
        sender.submit(0, SECTOR_SIZE, 1).unwrap();
        sender.submit(0, SECTOR_SIZE, 2).unwrap();
        drop(sender);

        let calls = Arc::new(AtomicUsize::new(0));
        let worker_calls = Arc::clone(&calls);
        let handle = thread::spawn(move || {
            worker.run_with(move |_, _, _| {
                worker_calls.fetch_add(1, Ordering::SeqCst);
                Ok(())
            })
        });
        handle.join().unwrap().unwrap();
        assert_eq!(calls.load(Ordering::SeqCst), 0);
    }

    #[test]
    fn running_discard_finishes_and_queued_ones_are_skipped_after_close() {
        let (sender, _notifier) = new_queue(4, 16);
        let worker = sender.worker(owned_fd());
        sender.submit(0, SECTOR_SIZE, 1).unwrap();
        sender.submit(0, SECTOR_SIZE, 2).unwrap();

        let (started_tx, started_rx) = mpsc::channel();
        let (release_tx, release_rx) = mpsc::channel::<()>();
        let calls = Arc::new(AtomicUsize::new(0));
        let worker_calls = Arc::clone(&calls);
        let handle = thread::spawn(move || {
            worker.run_with(move |_, _, _| {
                let call = worker_calls.fetch_add(1, Ordering::SeqCst);
                if call == 0 {
                    started_tx.send(()).unwrap();
                    release_rx.recv().unwrap();
                }
                Ok(())
            })
        });

        started_rx.recv_timeout(Duration::from_secs(5)).unwrap();
        // The first discard is executing; it is still counted as outstanding.
        assert_eq!(outstanding(&sender), 2);
        assert_eq!(completions(&sender), 0);

        drop(sender);
        release_tx.send(()).unwrap();
        handle.join().unwrap().unwrap();

        // Only the in-flight ioctl ran; the queued second task was skipped.
        assert_eq!(calls.load(Ordering::SeqCst), 1);
    }

    #[test]
    fn saturated_notifier_retains_the_completion() {
        let (sender, notifier) = new_queue(4, 16);
        assert!(notifier.write(u64::MAX - 1).is_ok());
        while notifier.write(1).is_ok() {}
        let worker = sender.worker(owned_fd());

        let handle = thread::spawn(move || worker.run_with(|_, _, _| Ok(())));
        sender.submit(0, SECTOR_SIZE, 7).unwrap();
        wait_until("the completion to be retained", || {
            completions(&sender) == 1
        });
        // EAGAIN from the already-readable notifier is not fatal: the retained
        // completion is still drained.
        assert_eq!(sender.drain(), vec![(7, 0)]);
        drop(sender);
        handle.join().unwrap().unwrap();
    }

    #[test]
    fn owns_its_fd_after_the_original_closes() {
        let temp = TempFile::new().unwrap();
        // Learn the errno the kernel returns for this regular file so the
        // assertion stays honest on kernels that differ.
        let probe = temp.as_file().try_clone().unwrap();
        let expected = block_discard(probe.as_raw_fd(), 0, 4 * SECTOR_SIZE)
            .unwrap_err()
            .raw_os_error()
            .unwrap();
        assert_ne!(expected, libc::EBADF);

        let owned: OwnedFd = temp.as_file().try_clone().unwrap().into();
        let notifier = Arc::new(EventFd::new(libc::EFD_NONBLOCK).unwrap());
        let sender = DiscardSender::new(4, 16, notifier);
        let worker = sender.worker(owned);
        drop(temp);

        let handle = thread::spawn(move || worker.run());
        sender.submit(0, 4 * SECTOR_SIZE, 11).unwrap();
        wait_until("the discard on the owned fd to complete", || {
            completions(&sender) == 1
        });
        assert_eq!(sender.drain(), vec![(11, -expected)]);
        drop(sender);
        handle.join().unwrap().unwrap();
    }
}
