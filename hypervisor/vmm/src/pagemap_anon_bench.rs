// Copyright © 2026 Tencent Corporation
//
// SPDX-License-Identifier: Apache-2.0

//! Privileged micro-benchmark for the pagemap bit-61 optimization.
//!
//! Kept separate from `pagemap_anon.rs` so the production implementation and
//! its ordinary unit tests are not obscured by benchmark-only scaffolding.
//!
//! Ignored by default (`CAP_SYS_ADMIN` is required to open `/proc/kpageflags`).
//! Run with `--ignored`. Optional env: `CUBE_PAGEMAP_BENCH_MIB` (comma-separated
//! sizes, default `64,256,1024`) and `CUBE_PAGEMAP_BENCH_ITERS` (default `7`).

use super::*;
use std::hint::black_box;
use std::io::Write;
use std::os::unix::io::AsRawFd;
use std::time::{Instant, SystemTime, UNIX_EPOCH};

struct PrivateFileMapping {
    ptr: *mut libc::c_void,
    length: usize,
}

impl PrivateFileMapping {
    fn new(size_mib: u64, page_size: u64) -> Self {
        let length = size_mib * 1024 * 1024;
        let path = std::env::temp_dir().join(format!(
            "cube_pagemap_bench_{}_{}",
            std::process::id(),
            SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        let mut file = std::fs::OpenOptions::new()
            .read(true)
            .write(true)
            .create_new(true)
            .open(&path)
            .expect("create benchmark backing file");

        let pattern = vec![0x5a; 1024 * 1024];
        for _ in 0..size_mib {
            file.write_all(&pattern)
                .expect("populate benchmark backing file");
        }

        let ptr = unsafe {
            libc::mmap(
                std::ptr::null_mut(),
                length as usize,
                libc::PROT_READ | libc::PROT_WRITE,
                libc::MAP_PRIVATE,
                file.as_raw_fd(),
                0,
            )
        };
        assert_ne!(ptr, libc::MAP_FAILED, "MAP_PRIVATE fixture failed");
        std::fs::remove_file(path).expect("unlink benchmark backing file");

        // Restore-like workload: read every file page, then CoW-write 10%.
        let base = ptr as *mut u8;
        let pages = length / page_size;
        for page in 0..pages {
            unsafe {
                black_box(std::ptr::read_volatile(
                    base.add((page * page_size) as usize),
                ));
            }
        }
        for page in (0..pages).step_by(10) {
            unsafe {
                std::ptr::write_volatile(base.add((page * page_size) as usize), 0xc2);
            }
        }

        Self {
            ptr,
            length: length as usize,
        }
    }
}

impl Drop for PrivateFileMapping {
    fn drop(&mut self) {
        unsafe {
            libc::munmap(self.ptr, self.length);
        }
    }
}

fn median_ns(mut samples: Vec<u128>) -> u128 {
    samples.sort_unstable();
    samples[samples.len() / 2]
}

fn measure(iterations: usize, mut scan: impl FnMut() -> Vec<bool>) -> u128 {
    black_box(scan()); // warmup
    median_ns(
        (0..iterations)
            .map(|_| {
                let start = Instant::now();
                black_box(scan());
                start.elapsed().as_nanos()
            })
            .collect(),
    )
}

fn env_values(name: &str, default: &str) -> Vec<u64> {
    std::env::var(name)
        .unwrap_or_else(|_| default.to_string())
        .split(',')
        .map(|value| value.trim().parse().expect("invalid benchmark setting"))
        .collect()
}

/// Compare the actual old and new VMM implementations on the same mapping.
#[test]
#[ignore = "requires CAP_SYS_ADMIN; cargo test -- --ignored (CUBE_PAGEMAP_BENCH_MIB / CUBE_PAGEMAP_BENCH_ITERS)"]
fn benchmark_get_anon_pages_before_after() {
    let page_size = host_page_size();
    let iterations = env_values("CUBE_PAGEMAP_BENCH_ITERS", "7")[0] as usize;
    println!(
        "pagemap_anon release micro-benchmark: page_size={page_size}, iterations={iterations}"
    );
    println!(
        "{:<8} {:>12} {:>12} {:>10} {:>12} {:>12} {:>10} {:>10}",
        "MiB", "old_ms", "new_ms", "speedup", "old_pages", "new_pages", "under", "over"
    );

    for size_mib in env_values("CUBE_PAGEMAP_BENCH_MIB", "64,256,1024") {
        let mapping = PrivateFileMapping::new(size_mib, page_size);
        let host_addr = mapping.ptr as u64;
        let length = mapping.length as u64;

        let old_ns = measure(iterations, || {
            scan_kpageflags_anon(host_addr, length)
                .expect("kpageflags scan failed")
                .0
        });
        let new_ns = measure(iterations, || {
            scan_pagemap_cow_anon(host_addr, length)
                .expect("bit61 scan failed")
                .0
        });

        let old = scan_kpageflags_anon(host_addr, length)
            .expect("kpageflags comparison failed")
            .0;
        let new = scan_pagemap_cow_anon(host_addr, length)
            .expect("bit61 comparison failed")
            .0;
        let under = old.iter().zip(&new).filter(|(a, b)| **a && !**b).count();
        let over = old.iter().zip(&new).filter(|(a, b)| !**a && **b).count();
        let old_pages = old.iter().filter(|&&save| save).count();
        let new_pages = new.iter().filter(|&&save| save).count();

        println!(
            "{:<8} {:>12.3} {:>12.3} {:>9.1}x {:>12} {:>12} {:>10} {:>10}",
            size_mib,
            old_ns as f64 / 1_000_000.0,
            new_ns as f64 / 1_000_000.0,
            old_ns as f64 / new_ns as f64,
            old_pages,
            new_pages,
            under,
            over
        );
        assert_eq!(under, 0, "bit61 path under-saved legacy KPF_ANON pages");
    }
}
