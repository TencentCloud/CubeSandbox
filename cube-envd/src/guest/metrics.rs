// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use std::collections::HashMap;
use std::time::{SystemTime, UNIX_EPOCH};
use tokio::sync::Mutex;

#[derive(Default)]
pub(crate) struct Metrics {
    previous_cpu: Mutex<(u64, u64)>,
}

impl Metrics {
    pub(crate) async fn sample(&self) -> std::io::Result<serde_json::Value> {
        let memory = tokio::fs::read_to_string("/proc/meminfo").await?;
        let memory: HashMap<_, _> = memory
            .lines()
            .filter_map(|line| {
                let (key, value) = line.split_once(':')?;
                Some((
                    key,
                    value.split_whitespace().next()?.parse::<u64>().ok()? * 1024,
                ))
            })
            .collect();
        let value = |key| memory.get(key).copied().unwrap_or(0);
        let total = value("MemTotal");
        let cache = value("Cached") + value("SReclaimable");
        let available = memory
            .get("MemAvailable")
            .copied()
            .unwrap_or(value("MemFree") + cache);
        let used = total.saturating_sub(available);
        let mut previous = self.previous_cpu.lock().await;
        let cpu = tokio::fs::read_to_string("/proc/stat").await?;
        let times: Vec<u64> = cpu
            .lines()
            .next()
            .unwrap_or("")
            .split_whitespace()
            .skip(1)
            .take(8)
            .filter_map(|v| v.parse().ok())
            .collect();
        if times.len() < 5 {
            return Err(std::io::Error::other("incomplete CPU sample"));
        }
        let total_cpu: u64 = times.iter().sum();
        let busy_cpu = total_cpu - times[3] - times[4];
        let percent = {
            let delta = total_cpu.saturating_sub(previous.0);
            let busy = busy_cpu.saturating_sub(previous.1);
            *previous = (total_cpu, busy_cpu);
            if delta == 0 {
                0.0
            } else {
                ((busy as f64 / delta as f64 * 10000.0).round() / 100.0).min(100.0)
            }
        };
        drop(previous);
        let cpu_count = cpu
            .lines()
            .filter(|line| {
                line.strip_prefix("cpu")
                    .is_some_and(|v| v.starts_with(|c: char| c.is_ascii_digit()))
            })
            .count();
        let mut disk = std::mem::MaybeUninit::<libc::statfs>::uninit();
        if unsafe { libc::statfs(c"/".as_ptr(), disk.as_mut_ptr()) } != 0 {
            return Err(std::io::Error::last_os_error());
        }
        let disk = unsafe { disk.assume_init() };
        let disk_total = disk.f_blocks * disk.f_bsize as u64;
        let disk_used = disk_total - disk.f_bavail * disk.f_bsize as u64;
        Ok(serde_json::json!({
            "ts": SystemTime::now().duration_since(UNIX_EPOCH).unwrap_or_default().as_secs(),
            "cpu_count": cpu_count, "cpu_used_pct": percent,
            "mem_total": total, "mem_used": used, "mem_cache": cache,
            "mem_total_mib": total / 1024 / 1024, "mem_used_mib": used / 1024 / 1024,
            "disk_total": disk_total, "disk_used": disk_used,
        }))
    }
}
