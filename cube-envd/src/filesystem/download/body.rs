// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! The `/files` body pipeline: how bytes get from the file to the socket.
//!
//! Go serves a download with one kernel-side `sendfile` loop; a userspace body
//! cannot reach that, so this pipeline buys the equivalent the other way: as few
//! chunks as possible, read and written concurrently, over recycled buffers, and
//! with explicit budgets for what a client that stops reading may cost.
//!
//! ```text
//! read -> pooled buffer -> bounded channel -> socket
//!           ^ one producer task per body, three shapes:
//!             1. blocking producer, body-sized slices + read-ahead, <= pool/4
//!                bodies (one pool thread each: the fast shape)
//!             2. async producer, same slices, <= pool/2 bodies in total
//!             3. async producer, 256 KiB slices, no read-ahead (the rest)
//! ```
//!
//! A body that fits in one chunk never reaches a producer: it is a single read
//! (see `reader_stream_with`). Nothing waits for a budget — a body that misses
//! one degrades to the next shape, because queueing behind a stalled client is
//! the failure mode the budgets exist to remove. A third, global budget (the
//! "in-flight" cap in `platform/limits.rs`, taken by the handler so it can
//! answer `503`) bounds how many large bodies exist at all.
//!
//! The constants below are measured, not guessed: 1 MiB halves the read and
//! wakeup count of 512 KiB and doubles the buffers in flight (343 -> 216 daemon
//! syscalls per 32 MiB body against 1102 before the pool), and 256 KiB is what
//! the unbuffered shape streams in so a storm of stalled downloads cannot grow
//! memory with the connection count (~1.3 MiB per body instead of ~5 MiB).
//!
//! The chunk is a ceiling, not a constant: [`chunk_for`] scales it down with the
//! body, because one body holds `READ_AHEAD + 2` buffers at once, so a fixed
//! 1 MiB slice made every connection carry ~4 MiB of buffer capacity no matter
//! how little it sent — thirty-two concurrent 4 MiB downloads turned that into
//! 211 -> 224 MiB of peak RSS (against 21 MiB for the old 64 KiB reader and
//! 15.7 MiB for Go) with no throughput to show for it. The buffers themselves
//! are recycled through one process-wide [`ReadPool`], so the chunk also decides
//! how much of that pool one body occupies.

/// Read size for a buffered body (see the module doc for the measurements).
pub(super) const DOWNLOAD_CHUNK: usize = 1024 * 1024;

/// How far the buffered reader runs ahead of the socket: enough to keep the
/// read off the write's critical path, no deeper (the socket is the slow side).
pub(super) const DOWNLOAD_READ_AHEAD: usize = 2;

/// Slice size for the unbuffered shape.
pub(super) const DOWNLOAD_STREAM_SLICE: usize = 256 * 1024;

/// Read size for one body: a sixteenth of its length, clamped to
/// [`DOWNLOAD_STREAM_SLICE`]..[`DOWNLOAD_CHUNK`].
///
/// A body holds `READ_AHEAD + 2` buffers in flight at once, so a fixed 1 MiB
/// chunk costs every connection ~4 MiB of buffer capacity whatever it is
/// sending. Sizing the chunk to the body keeps that footprint proportional to
/// the body (1 MiB of buffers for a 4 MiB file) and still gives large bodies
/// the full chunk, where the syscall measurements say it pays. A `None` limit
/// (a body of unknown length) has nothing to scale by and keeps the ceiling.
pub(super) fn chunk_for(limit: Option<u64>) -> usize {
    match limit {
        Some(n) => ((n / 16) as usize).clamp(DOWNLOAD_STREAM_SLICE, DOWNLOAD_CHUNK),
        None => DOWNLOAD_CHUNK,
    }
}

/// A read buffer that recycles itself into its pool once the last `Bytes`
/// slice of it is dropped.
pub(super) struct PooledBuffer {
    pool: ReadPool,
    buf: Vec<u8>,
}

impl AsRef<[u8]> for PooledBuffer {
    fn as_ref(&self) -> &[u8] {
        &self.buf
    }
}

impl Drop for PooledBuffer {
    fn drop(&mut self) {
        self.pool.recycle(std::mem::take(&mut self.buf));
    }
}

/// The pool's byte budget comes from the configured budgets — one read buffer
/// per buffered body, `platform::limits::download_pool_bytes()` — not from a
/// constant here: 4 MiB at `-blocking-threads 8`, 32 MiB at the default pool and
/// never more than that, so the cache follows configuration downwards and stays
/// bounded upwards.
///
/// The pool is what keeps a body from faulting in fresh pages: measured in a
/// 2 vCPU guest at 32 concurrent 4 MiB downloads, allocating per body cost
/// **73.6 minor faults per MiB** against 4.7 for a reader that reuses one
/// buffer for the whole body and 0.1 for Go's `sendfile`, at roughly 25 us of
/// daemon CPU per fault.
///
/// Recycled read buffers. A fresh allocation per read costs an `mmap`, a
/// `munmap` and a page-faulting zero-fill of the whole chunk — under musl that
/// was 278 syscalls per 32 MiB download (142 `mmap` + 136 `munmap`), a quarter
/// of the whole path's budget — and doing it once per *body* pays the page
/// faults again on every request.
struct PoolInner {
    free: std::sync::Mutex<Vec<Vec<u8>>>,
    retained: std::sync::atomic::AtomicUsize,
    budget: usize,
}

#[derive(Clone)]
pub(super) struct ReadPool(std::sync::Arc<PoolInner>);

impl ReadPool {
    /// A pool holding at most `budget` bytes of recycled buffers.
    pub(super) fn new(budget: usize) -> ReadPool {
        ReadPool(std::sync::Arc::new(PoolInner {
            free: Default::default(),
            retained: std::sync::atomic::AtomicUsize::new(0),
            budget,
        }))
    }

    /// A buffer of at least `len` bytes and at most `max` bytes — the caller's
    /// size class, so a 256 KiB body cannot take a 1 MiB buffer and hold it for
    /// its life. The smallest recycled one that fits, or a fresh allocation.
    ///
    /// Callers index `buf[..want]`, so the filter is on `len()`, not
    /// `capacity()`; `recycle` only keeps buffers where the two are equal, which
    /// the assert pins down.
    pub(super) fn take(&self, len: usize, max: usize) -> Vec<u8> {
        let len = len.clamp(1, max.max(1));
        // A poisoned lock only means some *other* download's body task
        // panicked; the buffers themselves are plain bytes.
        let mut free = lock(&self.0.free);
        let fit = free
            .iter()
            .enumerate()
            .filter(|(_, buf)| buf.len() >= len && buf.len() <= max)
            .min_by_key(|(_, buf)| buf.len())
            .map(|(i, _)| i);
        if let Some(i) = fit {
            let buf = free.swap_remove(i);
            debug_assert_eq!(buf.len(), buf.capacity(), "pooled buffers are exact");
            self.0
                .retained
                .fetch_sub(buf.capacity(), std::sync::atomic::Ordering::Relaxed);
            return buf;
        }
        drop(free);
        vec![0u8; len]
    }

    /// `Bytes` over the first `n` bytes of `buf`, recycled when dropped.
    pub(super) fn bytes(&self, buf: Vec<u8>, n: usize) -> bytes::Bytes {
        bytes::Bytes::from_owner(PooledBuffer {
            pool: self.clone(),
            buf,
        })
        .slice(..n)
    }

    /// Keep a buffer for the next body while the pool is under its budget.
    ///
    /// Only exact-size buffers are kept (`len == capacity`), which is what lets
    /// `take` filter on `len()` and lets callers index `buf[..want]` safely.
    fn recycle(&self, buf: Vec<u8>) {
        let cap = buf.capacity();
        if cap == 0 || cap > DOWNLOAD_CHUNK || buf.len() != cap {
            return;
        }
        let mut free = lock(&self.0.free);
        if self.0.retained.load(std::sync::atomic::Ordering::Relaxed) + cap > self.0.budget {
            return;
        }
        self.0
            .retained
            .fetch_add(cap, std::sync::atomic::Ordering::Relaxed);
        free.push(buf);
    }

    /// Bytes parked in the free list.
    #[cfg(test)]
    pub(super) fn retained(&self) -> usize {
        self.0.retained.load(std::sync::atomic::Ordering::Relaxed)
    }

    #[cfg(test)]
    pub(super) fn budget(&self) -> usize {
        self.0.budget
    }

    #[cfg(test)]
    pub(super) fn free_len(&self) -> usize {
        lock(&self.0.free).len()
    }
}

/// The process-wide pool every body recycles through.
pub(super) fn pool() -> ReadPool {
    static POOL: std::sync::OnceLock<ReadPool> = std::sync::OnceLock::new();
    POOL.get_or_init(|| ReadPool::new(crate::platform::limits::download_pool_bytes(DOWNLOAD_CHUNK)))
        .clone()
}

fn lock<T>(mutex: &std::sync::Mutex<T>) -> std::sync::MutexGuard<'_, T> {
    match mutex.lock() {
        Ok(guard) => guard,
        Err(poisoned) => poisoned.into_inner(),
    }
}

/// The two global budgets: `blocking` bounds pinned pool threads, `buffered`
/// bounds memory (a permit buys 1 MiB slices with read-ahead). Passed in so
/// tests can drive the shapes without the process-wide semaphores.
#[derive(Clone)]
pub(super) struct Budgets {
    pub(super) blocking: std::sync::Arc<tokio::sync::Semaphore>,
    pub(super) buffered: std::sync::Arc<tokio::sync::Semaphore>,
}

/// Permits held by one body's producer; dropping them returns the budgets.
pub(super) struct BudgetGuard {
    pub(super) _in_flight: Option<tokio::sync::OwnedSemaphorePermit>,
    pub(super) _blocking: Option<tokio::sync::OwnedSemaphorePermit>,
    pub(super) _buffered: Option<tokio::sync::OwnedSemaphorePermit>,
}

static BUDGETS: std::sync::OnceLock<Budgets> = std::sync::OnceLock::new();

/// Global cap on concurrent large downloads (`platform/limits.rs`). Unlike the
/// tier budgets this one is acquired by the *handler*, because a request over
/// the cap must be refused with a status code the body stream cannot produce.
static IN_FLIGHT: std::sync::OnceLock<std::sync::Arc<tokio::sync::Semaphore>> =
    std::sync::OnceLock::new();

pub(super) fn in_flight_budget() -> std::sync::Arc<tokio::sync::Semaphore> {
    IN_FLIGHT
        .get_or_init(|| {
            std::sync::Arc::new(tokio::sync::Semaphore::new(
                crate::platform::limits::download_max_bodies(),
            ))
        })
        .clone()
}

/// Take a global slot for a body that is *not* a single read: `Ok(None)` means
/// the body is exempt (it costs one read), `Ok(Some(permit))` holds a slot until
/// the body ends, `Err(())` means the handler must refuse with `503` — never
/// queue.
#[allow(clippy::result_unit_err)] // `()` is the whole verdict; the caller owns the 503 shape
pub(super) fn acquire_in_flight(
    limit: Option<u64>,
    budget: &std::sync::Arc<tokio::sync::Semaphore>,
) -> Result<Option<tokio::sync::OwnedSemaphorePermit>, ()> {
    if matches!(limit, Some(n) if n <= DOWNLOAD_CHUNK as u64) {
        return Ok(None);
    }
    budget.clone().try_acquire_owned().map(Some).map_err(|_| ())
}

pub(super) fn budgets() -> Budgets {
    BUDGETS
        .get_or_init(|| Budgets {
            blocking: std::sync::Arc::new(tokio::sync::Semaphore::new(
                crate::platform::limits::download_blocking_producers(),
            )),
            buffered: std::sync::Arc::new(tokio::sync::Semaphore::new(
                crate::platform::limits::download_buffered_bodies(),
            )),
        })
        .clone()
}

/// Chunked reader stream. `limit` bounds the total bytes produced (single-range
/// 206 bodies); `None` streams to EOF.
pub(super) async fn reader_stream(
    file: tokio::fs::File,
    limit: Option<u64>,
    in_flight: Option<tokio::sync::OwnedSemaphorePermit>,
) -> futures::stream::BoxStream<'static, Result<bytes::Bytes, std::io::Error>> {
    reader_stream_with(budgets(), file, limit, in_flight).await
}

/// The body pipeline with the budgets passed in. Small bodies keep the plain
/// single-read shape; larger ones take shape 1, 2 or 3 (module doc).
pub(super) async fn reader_stream_with(
    budgets: Budgets,
    mut file: tokio::fs::File,
    limit: Option<u64>,
    in_flight: Option<tokio::sync::OwnedSemaphorePermit>,
) -> futures::stream::BoxStream<'static, Result<bytes::Bytes, std::io::Error>> {
    use futures::StreamExt;
    use tokio::io::AsyncReadExt;

    if let Some(n) = limit {
        if n <= DOWNLOAD_CHUNK as u64 {
            return futures::stream::once(async move {
                let mut buf = vec![0u8; n as usize];
                match file.read(&mut buf).await {
                    // A short read (the file shrank under us) is the whole
                    // body; an empty one is no body at all, like EOF.
                    Ok(0) => None,
                    Ok(read) => {
                        buf.truncate(read);
                        Some(Ok(bytes::Bytes::from(buf)))
                    }
                    Err(e) => Some(Err(e)),
                }
            })
            .filter_map(futures::future::ready)
            .boxed();
        }
    }

    // Never `acquire().await` on either budget: waiting for a permit would turn
    // a saturated budget into head-of-line blocking behind a stalled body.
    // Degrading to the next tier is slower but bounded.
    let buffered = match budgets.buffered.try_acquire_owned() {
        Ok(permit) => permit,
        Err(_) => {
            // Tier 3: no buffered slot, so stream 256 KiB slices without
            // read-ahead and hold nothing else.
            let pool = pool();
            let (tx, rx) = tokio::sync::mpsc::channel(1);
            tokio::spawn(read_ahead(
                file,
                limit,
                pool,
                tx,
                DOWNLOAD_STREAM_SLICE,
                BudgetGuard {
                    _in_flight: in_flight,
                    _blocking: None,
                    _buffered: None,
                },
            ));
            return tokio_stream::wrappers::ReceiverStream::new(rx).boxed();
        }
    };
    let chunk = chunk_for(limit);
    let pool = pool();
    let (tx, rx) = tokio::sync::mpsc::channel(DOWNLOAD_READ_AHEAD);
    match budgets.blocking.try_acquire_owned() {
        Ok(blocking) => {
            // `into_std` waits for any in-flight operation on the tokio handle;
            // from here the producer owns the fd and does plain blocking reads.
            let std_file = file.into_std().await;
            let guard = BudgetGuard {
                _in_flight: in_flight,
                _blocking: Some(blocking),
                _buffered: Some(buffered),
            };
            tokio::task::spawn_blocking(move || {
                read_ahead_blocking(std_file, limit, pool, tx, chunk, guard)
            });
        }
        Err(_) => {
            let guard = BudgetGuard {
                _in_flight: in_flight,
                _blocking: None,
                _buffered: Some(buffered),
            };
            tokio::spawn(read_ahead(file, limit, pool, tx, chunk, guard));
        }
    }
    tokio_stream::wrappers::ReceiverStream::new(rx).boxed()
}

/// Shape 1: one pool crossing for the whole body, plain blocking reads. Holds
/// `_guard` until the body ends, so the budgets bound resources in use.
fn read_ahead_blocking(
    mut file: std::fs::File,
    mut limit: Option<u64>,
    pool: ReadPool,
    tx: tokio::sync::mpsc::Sender<Result<bytes::Bytes, std::io::Error>>,
    chunk: usize,
    _guard: BudgetGuard,
) {
    use std::io::Read;
    loop {
        let want = match limit {
            Some(0) => return,
            Some(r) => r.min(chunk as u64) as usize,
            None => chunk,
        };
        let mut buf = pool.take(want, chunk);
        match file.read(&mut buf[..want]) {
            // EOF: hand the buffer back before leaving, or every body sends one
            // buffer to the allocator instead of the pool.
            Ok(0) => {
                pool.recycle(buf);
                return;
            }
            Ok(n) => {
                if let Some(r) = limit.as_mut() {
                    *r -= n as u64;
                }
                if tx.blocking_send(Ok(pool.bytes(buf, n))).is_err() {
                    // The body was dropped (client gone, or the response ended
                    // early) — stop reading rather than fill the channel.
                    return;
                }
            }
            Err(e) => {
                pool.recycle(buf);
                let _ = tx.blocking_send(Err(e));
                return;
            }
        }
    }
}

/// Shapes 2 and 3: the same loop as an async task, so a stalled body parks on
/// the channel instead of on a pool thread. The caller picks the slice size and
/// the channel depth.
pub(super) async fn read_ahead(
    mut file: tokio::fs::File,
    mut limit: Option<u64>,
    pool: ReadPool,
    tx: tokio::sync::mpsc::Sender<Result<bytes::Bytes, std::io::Error>>,
    chunk: usize,
    _guard: BudgetGuard,
) {
    use tokio::io::AsyncReadExt;
    loop {
        let want = match limit {
            Some(0) => return,
            Some(r) => r.min(chunk as u64) as usize,
            None => chunk,
        };
        let mut buf = pool.take(want, chunk);
        match file.read(&mut buf[..want]).await {
            Ok(0) => {
                pool.recycle(buf);
                return;
            }
            Ok(n) => {
                if let Some(r) = limit.as_mut() {
                    *r -= n as u64;
                }
                if tx.send(Ok(pool.bytes(buf, n))).await.is_err() {
                    // The body was dropped (client gone, or the response ended
                    // early) — stop reading rather than fill the channel.
                    return;
                }
            }
            Err(e) => {
                pool.recycle(buf);
                let _ = tx.send(Err(e)).await;
                return;
            }
        }
    }
}
