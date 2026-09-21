// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! 阻塞策略的手工测点（默认 `#[ignore]`，不进 CI）。
//!
//! 为什么需要它：cube-envd 每个沙箱常驻一个，`tokio::fs` 的"每次 syscall 一次线程池
//! 穿越"与"每请求一次 `spawn_blocking` + 同步 `std::fs`"之间的取舍，只有在同一台机器上
//! 量出来才有意义。跑法：
//!
//! ```bash
//! cargo test --locked --test blocking_strategy -- --ignored --nocapture
//! ```
//!
//! 结论写在 `src/filesystem/entries.rs` 的模块文档与 README 的 Development Notes 里；
//! 换实现或换运行时配置时重跑本测点。

use std::time::{Duration, Instant};

use tempfile::tempdir;

/// 每个测点的迭代次数：足够摊平抖动，又不至于让手工测点跑太久。
const ROUNDS: usize = 2000;

/// 取中位数，避免个别调度尖峰主导结论。
fn median(mut samples: Vec<Duration>) -> Duration {
    samples.sort();
    samples[samples.len() / 2]
}

// 比较 tokio::fs 与 spawn_blocking+std::fs 的单次开销。
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "manual measurement; run with --ignored --nocapture"]
async fn measure_tokio_fs_versus_spawn_blocking() {
    let directory = tempdir().expect("tempdir");
    let path = directory.path().join("probe");
    std::fs::write(&path, b"x").expect("seed probe file");

    // 预热：让线程池与页缓存先就位。
    for _ in 0..50 {
        let _ = tokio::fs::metadata(&path).await;
    }

    let mut tokio_samples = Vec::with_capacity(ROUNDS);
    for _ in 0..ROUNDS {
        let started = Instant::now();
        tokio::fs::metadata(&path).await.expect("tokio metadata");
        tokio_samples.push(started.elapsed());
    }

    let mut blocking_samples = Vec::with_capacity(ROUNDS);
    for _ in 0..ROUNDS {
        let target = path.clone();
        let started = Instant::now();
        tokio::task::spawn_blocking(move || std::fs::metadata(&target))
            .await
            .expect("join blocking task")
            .expect("blocking metadata");
        blocking_samples.push(started.elapsed());
    }

    let tokio_median = median(tokio_samples);
    let blocking_median = median(blocking_samples);
    println!(
        "tokio::fs metadata:            median {:?}/call over {ROUNDS} calls",
        tokio_median
    );
    println!(
        "spawn_blocking + std::fs:      median {:?}/call over {ROUNDS} calls",
        blocking_median
    );
    println!(
        "ratio (spawn_blocking / tokio::fs): {:.2}x",
        blocking_median.as_nanos() as f64 / tokio_median.as_nanos() as f64
    );
}
