// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Unit tests for the body pipeline (`super::body`). A child module of
//! `download`, so it can reach the pipeline's internals without widening their
//! visibility to the crate; the HTTP-level cases live in
//! `filesystem/data_plane_tests.rs`.

use super::body::*;
use futures::StreamExt;

/// Deterministic, position-dependent content: a chunk sliced from the
/// wrong offset of a pooled buffer still has the right length.
fn write_pattern(path: &std::path::Path, size: usize) {
    let data: Vec<u8> = (0..size).map(|i| (i % 251) as u8).collect();
    std::fs::write(path, data).unwrap();
}

/// Generous budgets, injected so no test depends on the process-wide ones.
fn test_budgets() -> Budgets {
    Budgets {
        blocking: std::sync::Arc::new(tokio::sync::Semaphore::new(8)),
        buffered: std::sync::Arc::new(tokio::sync::Semaphore::new(16)),
    }
}

async fn collect(
    file: tokio::fs::File,
    limit: Option<u64>,
) -> Vec<Result<bytes::Bytes, std::io::Error>> {
    reader_stream_with(test_budgets(), file, limit, None)
        .await
        .collect()
        .await
}

/// The same, through the unbuffered tier: no permits, so the body streams
/// `DOWNLOAD_STREAM_SLICE` slices and the chunk sizes say so.
async fn collect_unbuffered(
    file: tokio::fs::File,
    limit: Option<u64>,
) -> Vec<Result<bytes::Bytes, std::io::Error>> {
    let budgets = Budgets {
        blocking: std::sync::Arc::new(tokio::sync::Semaphore::new(0)),
        buffered: std::sync::Arc::new(tokio::sync::Semaphore::new(0)),
    };
    reader_stream_with(budgets, file, limit, None)
        .await
        .collect()
        .await
}

fn total(chunks: &[Result<bytes::Bytes, std::io::Error>]) -> usize {
    chunks.iter().map(|c| c.as_ref().unwrap().len()).sum()
}

/// The single-chunk arm: one read, one item, exact bytes.
#[tokio::test]
async fn a_body_that_fits_in_one_chunk_is_one_read() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("small.bin");
    let size = 4096;
    write_pattern(&path, size);

    let file = tokio::fs::File::open(&path).await.unwrap();
    let chunks = collect(file, Some(size as u64)).await;
    assert_eq!(chunks.len(), 1);
    let chunk = chunks.into_iter().next().unwrap().unwrap();
    assert_eq!(chunk.len(), size);
    assert!(chunk.iter().enumerate().all(|(i, b)| *b == (i % 251) as u8));
}

/// The chunking is the point of `DOWNLOAD_CHUNK` and is invisible to any
/// content assertion, so pin it: a regression to smaller reads would still
/// pass every correctness test and only show up as sandbox throughput. One body
/// above the floor and one at the ceiling.
#[tokio::test]
async fn a_large_body_reads_in_chunks_and_stops_at_the_limit() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("large.bin");
    let size = DOWNLOAD_CHUNK + 4096;
    write_pattern(&path, size);

    // Full chunks in order, then the tail.
    let chunk = chunk_for(Some(size as u64));
    let mut want = vec![chunk; size / chunk];
    if size % chunk != 0 {
        want.push(size % chunk);
    }
    let file = tokio::fs::File::open(&path).await.unwrap();
    let chunks = collect(file, Some(size as u64)).await;
    let lens: Vec<usize> = chunks.iter().map(|c| c.as_ref().unwrap().len()).collect();
    assert_eq!(lens, want);
    let all: Vec<u8> = chunks
        .into_iter()
        .flat_map(|c| c.unwrap().to_vec())
        .collect();
    assert_eq!(all.len(), size);
    assert!(all.iter().enumerate().all(|(i, b)| *b == (i % 251) as u8));

    // A body long enough to hit the ceiling still reads a whole chunk at a time.
    let big = 16 * 1024 * 1024 + 4096;
    let big_path = dir.path().join("bigger.bin");
    write_pattern(&big_path, big);
    assert_eq!(chunk_for(Some(big as u64)), DOWNLOAD_CHUNK);
    let file = tokio::fs::File::open(&big_path).await.unwrap();
    let lens: Vec<usize> = collect(file, Some(big as u64))
        .await
        .iter()
        .map(|c| c.as_ref().unwrap().len())
        .collect();
    assert_eq!(lens.len(), big / DOWNLOAD_CHUNK + 1);
    assert_eq!(lens[lens.len() - 1], 4096);
    assert!(lens[..lens.len() - 1].iter().all(|n| *n == DOWNLOAD_CHUNK));

    // A limit inside the first chunk is a single read (206 with a short
    // range).
    let file = tokio::fs::File::open(&path).await.unwrap();
    let limited = collect(file, Some(4096)).await;
    assert_eq!(limited.len(), 1);
    assert_eq!(total(&limited), 4096);

    // A limit crossing the chunk boundary stops exactly at the limit.
    let file = tokio::fs::File::open(&path).await.unwrap();
    let crossed = collect(file, Some(DOWNLOAD_CHUNK as u64 + 10)).await;
    assert_eq!(total(&crossed), DOWNLOAD_CHUNK + 10);

    // An exhausted limit reads nothing at all.
    let file = tokio::fs::File::open(&path).await.unwrap();
    assert!(collect(file, Some(0)).await.is_empty());

    // No limit streams to EOF.
    let file = tokio::fs::File::open(&path).await.unwrap();
    assert_eq!(total(&collect(file, None).await), size);
}

/// A read error reaches the body instead of ending the stream silently.
/// Reading a directory fd fails with EISDIR; the limit forces the
/// pipelined reader.
#[tokio::test]
async fn read_errors_are_delivered() {
    let dir = tempfile::tempdir().unwrap();
    let file = tokio::fs::File::open(dir.path()).await.unwrap();
    let chunks = collect(file, Some(DOWNLOAD_CHUNK as u64 + 1)).await;
    assert_eq!(chunks.len(), 1);
    assert!(chunks.into_iter().next().unwrap().is_err());

    // The error return holds a buffer too, and must give it back.
    let pool = ReadPool::new(DOWNLOAD_CHUNK * 8);
    let (tx, mut rx) = tokio::sync::mpsc::channel(DOWNLOAD_READ_AHEAD);
    read_ahead(
        tokio::fs::File::open(dir.path()).await.unwrap(),
        Some(DOWNLOAD_CHUNK as u64 + 1),
        pool.clone(),
        tx,
        DOWNLOAD_CHUNK,
        BudgetGuard {
            _in_flight: None,
            _blocking: None,
            _buffered: None,
        },
    )
    .await;
    assert!(rx.recv().await.unwrap().is_err());
    drop(rx);
    assert_eq!(pool.free_len(), 1, "the error return recycled its buffer");
}

/// The pool is what keeps a 1 MiB chunk from paying an allocation per
/// read; pin all of it (recycle on last drop, reuse the same allocation, and
/// stay inside the byte budget).
#[test]
fn pooled_buffers_recycle_and_the_pool_stays_bounded() {
    let pool = ReadPool::new(256);
    let bytes = pool.bytes(pool.take(64, 64), 8);
    assert_eq!(bytes.len(), 8);
    let live = bytes.clone();
    drop(bytes);
    assert_eq!(
        pool.free_len(),
        0,
        "a live slice keeps the buffer out of the pool"
    );
    drop(live);
    assert_eq!(pool.free_len(), 1, "the last slice recycles the buffer");
    assert_eq!(pool.retained(), 64);

    // The whole point of a process-wide pool: the next body gets the same
    // allocation back instead of faulting in fresh pages.
    let again = pool.take(64, 64);
    assert_eq!(pool.free_len(), 0);
    assert_eq!(pool.retained(), 0);
    assert_eq!(again.capacity(), 64);

    // A body that needs more than the pool holds gets a fresh buffer without
    // disturbing the recycled one.
    let bigger = pool.take(512, 512);
    assert_eq!(bigger.capacity(), 512);
    assert_eq!(pool.free_len(), 0);

    // Six 64-byte buffers come back, but only four fit in 256 bytes.
    let held: Vec<bytes::Bytes> = (0..6).map(|_| pool.bytes(pool.take(64, 64), 1)).collect();
    drop(held);
    drop(again);
    drop(bigger);
    assert_eq!(pool.retained(), 256, "surplus buffers are dropped");
    assert_eq!(pool.free_len(), 4);
    assert!(pool.retained() <= pool.budget());
}

/// The pipeline's only new failure mode: the client goes away mid-body.
/// Dropping the body drops the receiver, the producer's `send` fails, and
/// the task must end with every buffer back in the pool. This is also the
/// property that keeps a stalled download off the shared blocking pool:
/// the producer parks on the channel, never on a thread.
#[tokio::test]
async fn dropping_the_body_stops_the_producer_and_recycles_its_buffers() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("large.bin");
    let size = DOWNLOAD_CHUNK * 4;
    write_pattern(&path, size);

    let file = tokio::fs::File::open(&path).await.unwrap();
    let pool = ReadPool::new(DOWNLOAD_CHUNK * 8);
    let (tx, mut rx) = tokio::sync::mpsc::channel(DOWNLOAD_READ_AHEAD);
    let producer = tokio::spawn(read_ahead(
        file,
        Some(size as u64),
        pool.clone(),
        tx,
        DOWNLOAD_CHUNK,
        BudgetGuard {
            _in_flight: None,
            _blocking: None,
            _buffered: None,
        },
    ));

    // Taking `READ_AHEAD + 1` chunks proves the producer ran and had
    // `READ_AHEAD` more queued or in hand; then the client "disconnects" by
    // dropping the receiver.
    let mut taken = Vec::new();
    for _ in 0..=DOWNLOAD_READ_AHEAD {
        let chunk = rx.recv().await.expect("chunk").unwrap();
        assert_eq!(chunk.len(), DOWNLOAD_CHUNK);
        taken.push(chunk);
    }
    drop(rx);
    tokio::time::timeout(std::time::Duration::from_secs(5), producer)
        .await
        .expect("the producer must stop once the body is dropped")
        .expect("the producer task must not panic");

    drop(taken);
    let free = pool.free_len();
    assert!(
        free >= DOWNLOAD_READ_AHEAD + 2,
        "every buffer came back (free = {free})"
    );
    assert!(
        pool.retained() <= pool.budget(),
        "the pool stayed inside its budget ({} <= {})",
        pool.retained(),
        pool.budget()
    );
}

/// Both budgets are what keep downloads from consuming the whole pool and
/// from buffering without bound: a permit is held for the life of a body,
/// the next body degrades instead of waiting, and the unbuffered tier still
/// delivers the exact bytes — in smaller slices, which is the observable
/// difference.
#[tokio::test]
async fn the_download_budgets_hold_and_degrade_instead_of_waiting() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("large.bin");
    // Long enough that a producer fills its channel and parks in `send`,
    // which is the stalled-client shape: a short body would finish and give
    // its permits back on its own.
    let size = DOWNLOAD_CHUNK * (DOWNLOAD_READ_AHEAD + 4);
    write_pattern(&path, size);

    // One blocking slot and two buffered slots.
    let budgets = Budgets {
        blocking: std::sync::Arc::new(tokio::sync::Semaphore::new(1)),
        buffered: std::sync::Arc::new(tokio::sync::Semaphore::new(2)),
    };
    // Not polled on purpose: these bodies stand in for stalled clients, so
    // they keep their permits while they are alive.
    let stalled_blocking = reader_stream_with(
        budgets.clone(),
        tokio::fs::File::open(&path).await.unwrap(),
        Some(size as u64),
        None,
    )
    .await;
    assert_eq!(budgets.blocking.available_permits(), 0);
    assert_eq!(
        budgets.buffered.available_permits(),
        1,
        "tier 1 took one buffered slot"
    );

    // Tier 2: no blocking slot left, but still buffered (1 MiB slices).
    let stalled_async = reader_stream_with(
        budgets.clone(),
        tokio::fs::File::open(&path).await.unwrap(),
        Some(size as u64),
        None,
    )
    .await;
    assert_eq!(budgets.buffered.available_permits(), 0);

    // Tier 3: nothing left, so the body must stream 256 KiB slices without
    // read-ahead and produce the exact bytes.
    let unbuffered = reader_stream_with(
        budgets.clone(),
        tokio::fs::File::open(&path).await.unwrap(),
        Some(size as u64),
        None,
    )
    .await;
    let chunks = unbuffered.collect::<Vec<_>>().await;
    assert_eq!(total(&chunks), size);
    assert!(chunks
        .iter()
        .all(|c| c.as_ref().unwrap().len() <= DOWNLOAD_STREAM_SLICE));
    assert!(chunks[0].as_ref().unwrap().len() <= DOWNLOAD_STREAM_SLICE);
    assert_eq!(
        budgets.blocking.available_permits(),
        0,
        "tier 3 takes no permit"
    );
    assert_eq!(
        budgets.buffered.available_permits(),
        0,
        "tier 3 takes no permit"
    );

    // Ending (or dropping) the stalled bodies gives their permits back, so
    // the next download gets the fast tier again.
    drop(stalled_blocking);
    drop(stalled_async);
    for _ in 0..200 {
        if budgets.blocking.available_permits() == 1 && budgets.buffered.available_permits() == 2 {
            break;
        }
        tokio::time::sleep(std::time::Duration::from_millis(10)).await;
    }
    assert_eq!(
        budgets.blocking.available_permits(),
        1,
        "blocking permit came back"
    );
    assert_eq!(
        budgets.buffered.available_permits(),
        2,
        "buffered permits came back"
    );
}

/// The global cap is what keeps a storm of stalled downloads from growing
/// the daemon's memory with the connection count: a body that is not a
/// single read holds a slot for its whole life, a small body never takes
/// one, and a request that misses out is refused instead of queued.
#[tokio::test]
async fn the_global_cap_refuses_rather_than_queues_and_small_bodies_are_exempt() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("large.bin");
    let size = DOWNLOAD_CHUNK * (DOWNLOAD_READ_AHEAD + 4);
    write_pattern(&path, size);

    let cap = std::sync::Arc::new(tokio::sync::Semaphore::new(1));

    // A small body is exempt even with the cap fully taken.
    let held = cap.clone().try_acquire_owned().unwrap();
    assert!(matches!(acquire_in_flight(Some(4096), &cap), Ok(None)));
    assert!(matches!(acquire_in_flight(Some(0), &cap), Ok(None)));
    drop(held);
    assert!(matches!(
        acquire_in_flight(Some(size as u64), &cap),
        Ok(Some(_))
    ));

    // A large body gets the last slot...
    let permit = match acquire_in_flight(Some(size as u64), &cap) {
        Ok(Some(permit)) => permit,
        _ => panic!("the last slot should have been available"),
    };
    // ... and the next one is refused, not queued.
    assert!(acquire_in_flight(Some(size as u64), &cap).is_err());
    assert!(
        acquire_in_flight(None, &cap).is_err(),
        "EOF bodies count too"
    );

    // The slot is held for the body's life and comes back with it.
    let body = reader_stream_with(
        test_budgets(),
        tokio::fs::File::open(&path).await.unwrap(),
        Some(size as u64),
        Some(permit),
    )
    .await;
    assert!(
        acquire_in_flight(Some(size as u64), &cap).is_err(),
        "still held"
    );
    assert_eq!(total(&body.collect::<Vec<_>>().await), size);
    for _ in 0..200 {
        if acquire_in_flight(Some(size as u64), &cap).is_ok() {
            return;
        }
        tokio::time::sleep(std::time::Duration::from_millis(10)).await;
    }
    panic!("the global slot never came back");
}

/// The unbuffered tier is only about footprint: the bytes are the same and
/// the limits are still honoured.
#[tokio::test]
async fn the_unbuffered_tier_streams_the_same_bytes() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("large.bin");
    let size = DOWNLOAD_STREAM_SLICE * 4 + 4096;
    write_pattern(&path, size);

    let file = tokio::fs::File::open(&path).await.unwrap();
    let chunks = collect_unbuffered(file, Some(size as u64)).await;
    assert!(
        chunks.len() >= 5,
        "4 slices plus a tail, got {}",
        chunks.len()
    );
    assert!(chunks
        .iter()
        .all(|c| c.as_ref().unwrap().len() <= DOWNLOAD_STREAM_SLICE));
    let all: Vec<u8> = chunks
        .into_iter()
        .flat_map(|c| c.unwrap().to_vec())
        .collect();
    assert_eq!(all.len(), size);
    assert!(all.iter().enumerate().all(|(i, b)| *b == (i % 251) as u8));

    // A limit inside the first slice is a single read.
    let file = tokio::fs::File::open(&path).await.unwrap();
    assert_eq!(total(&collect_unbuffered(file, Some(4096)).await), 4096);

    // A body that fits in one chunk never reaches the tiers at all.
    let file = tokio::fs::File::open(&path).await.unwrap();
    let small = collect_unbuffered(file, Some(DOWNLOAD_CHUNK as u64)).await;
    assert_eq!(small.len(), 1);
    assert_eq!(total(&small), DOWNLOAD_CHUNK);
}

/// A body must not send its buffers to the allocator on the way out: the
/// producer stops on `Ok(0)` (and on error) holding a buffer, and that buffer is
/// exactly the one the next body wants.
#[tokio::test]
async fn the_producer_recycles_the_buffer_it_stops_on() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("exact.bin");
    // Exact multiple of the chunk: the producer reads EOF on its third take.
    let size = DOWNLOAD_CHUNK * 2;
    write_pattern(&path, size);

    let pool = ReadPool::new(DOWNLOAD_CHUNK * 8);
    let (tx, mut rx) = tokio::sync::mpsc::channel(DOWNLOAD_READ_AHEAD);
    // `None` on purpose: with a known length the loop leaves through
    // `Some(0) => return` *before* taking a buffer, so the `Ok(0)` arm — the one
    // a shrinking file (or an unknown length) reaches — is only exercised here.
    let producer = tokio::spawn(read_ahead(
        tokio::fs::File::open(&path).await.unwrap(),
        None,
        pool.clone(),
        tx,
        DOWNLOAD_CHUNK,
        BudgetGuard {
            _in_flight: None,
            _blocking: None,
            _buffered: None,
        },
    ));
    let mut delivered = Vec::new();
    while let Some(chunk) = rx.recv().await {
        delivered.push(chunk.unwrap());
    }
    producer.await.unwrap();
    assert_eq!(delivered.len(), 2);
    drop(delivered);
    for _ in 0..200 {
        if pool.free_len() >= 3 {
            break;
        }
        tokio::time::sleep(std::time::Duration::from_millis(10)).await;
    }
    assert_eq!(
        pool.free_len(),
        3,
        "two delivered buffers plus the one the EOF return was holding"
    );

    // ... and with a known length the buffer flow is the same, minus the EOF
    // take: this is the shape every /files response uses.
    let pool = ReadPool::new(DOWNLOAD_CHUNK * 8);
    let (tx, mut rx) = tokio::sync::mpsc::channel(DOWNLOAD_READ_AHEAD);
    tokio::spawn(read_ahead(
        tokio::fs::File::open(&path).await.unwrap(),
        Some(size as u64),
        pool.clone(),
        tx,
        DOWNLOAD_CHUNK,
        BudgetGuard {
            _in_flight: None,
            _blocking: None,
            _buffered: None,
        },
    ))
    .await
    .unwrap();
    let mut delivered = Vec::new();
    while let Some(chunk) = rx.recv().await {
        delivered.push(chunk.unwrap());
    }
    drop(delivered);
    for _ in 0..200 {
        if pool.free_len() >= 2 {
            break;
        }
        tokio::time::sleep(std::time::Duration::from_millis(10)).await;
    }
    assert_eq!(pool.free_len(), 2, "both buffers came back");
}

/// `take` is bounded by the caller's size class: a 256 KiB body must not park a
/// 1 MiB buffer for its whole life, and the 1 MiB class must still find it.
#[test]
fn the_pool_keeps_size_classes_apart() {
    let mib = 1 << 20;
    let slice = DOWNLOAD_STREAM_SLICE;
    let pool = ReadPool::new(4 * mib);
    let big = pool.take(mib, mib);
    assert_eq!(big.len(), mib);
    drop(pool.bytes(big, 1));
    assert_eq!(pool.free_len(), 1);

    let small = pool.take(slice, slice);
    assert_eq!(small.len(), slice, "a fresh slice-sized buffer");
    assert_eq!(pool.free_len(), 1, "the 1 MiB buffer stays parked");

    let big_again = pool.take(mib, mib);
    assert_eq!(big_again.len(), mib, "the 1 MiB class still finds it");
    assert_eq!(pool.free_len(), 0);
}

/// The pool's budget is derived from the same knob as the body budgets, is
/// expressed in this pipeline's chunk, and is capped — the three properties the
/// startup line and the memory bound rely on.
#[test]
fn the_pool_budget_follows_the_body_budget_and_the_chunk() {
    let buffered = crate::platform::limits::download_buffered_bodies();
    assert_eq!(
        super::pool_budget_bytes(),
        crate::platform::limits::download_pool_bytes(DOWNLOAD_CHUNK)
    );
    assert_eq!(
        super::pool_budget_bytes(),
        buffered.min(32) * DOWNLOAD_CHUNK,
        "one buffer per buffered body, at most 32"
    );
    assert!(
        super::pool_budget_bytes() <= 32 << 20,
        "the pool has a ceiling"
    );
}

/// The chunk is the *ceiling* of the read size, not the read size: a body only a
/// few chunks long must not make its connection hold four 1 MiB buffers (the
/// 32 x 4 MiB RSS/tail regression found in review).
#[test]
fn the_buffered_chunk_scales_with_the_body() {
    assert_eq!(
        chunk_for(None),
        DOWNLOAD_CHUNK,
        "unknown length keeps 1 MiB"
    );
    assert_eq!(chunk_for(Some(4 << 20)), DOWNLOAD_STREAM_SLICE);
    assert_eq!(chunk_for(Some(8 << 20)), 512 * 1024);
    assert_eq!(chunk_for(Some(16 << 20)), DOWNLOAD_CHUNK);
    assert_eq!(chunk_for(Some(200 << 20)), DOWNLOAD_CHUNK);
    // Anything smaller than the floor is the floor, down to one byte.
    for n in [1, 4096, 1 << 20, 3 << 20] {
        assert_eq!(chunk_for(Some(n)), DOWNLOAD_STREAM_SLICE, "n = {n}");
    }
    assert!(chunk_for(Some(15 << 20)) <= DOWNLOAD_CHUNK);
}

/// A mid-size body takes the buffered shape but with body-sized buffers, and
/// the bytes are byte-for-byte what the fixed-chunk shape produced.
#[tokio::test]
async fn a_mid_size_body_buffers_with_body_sized_buffers() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("mid.bin");
    let size = 4 << 20;
    assert!(size > DOWNLOAD_CHUNK, "must be too long for a single read");
    write_pattern(&path, size);

    let file = tokio::fs::File::open(&path).await.unwrap();
    let chunks = collect(file, Some(size as u64)).await;
    assert!(
        chunks.len() >= 16,
        "a 4 MiB body reads in 256 KiB slices, got {}",
        chunks.len()
    );
    assert!(chunks
        .iter()
        .all(|c| c.as_ref().unwrap().len() <= DOWNLOAD_STREAM_SLICE));
    let all: Vec<u8> = chunks
        .into_iter()
        .flat_map(|c| c.unwrap().to_vec())
        .collect();
    assert_eq!(all.len(), size);
    assert!(all.iter().enumerate().all(|(i, b)| *b == (i % 251) as u8));
}
