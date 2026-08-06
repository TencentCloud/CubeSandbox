// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! GET /metrics — resource usage snapshot matching the upstream JSON shape:
//! {"ts","cpu_count","cpu_used_pct","mem_total_mib","mem_used_mib",
//!  "mem_total","mem_used","mem_cache","disk_used","disk_total"}

use std::sync::Arc;

use axum::extract::State;
use axum::http::{HeaderMap, StatusCode};
use axum::response::IntoResponse;

use crate::state::AppState;

pub async fn metrics(State(state): State<Arc<AppState>>, headers: HeaderMap) -> impl IntoResponse {
    if super::check_token(&state, &headers).is_err() {
        return StatusCode::UNAUTHORIZED.into_response();
    }
    let cpu = cpu_used_pct().await;
    let (mem_total, mem_available, mem_cache) = meminfo();
    let mem_used = mem_total.saturating_sub(mem_available);
    let (disk_total, disk_used) = disk_usage("/");
    let ts = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0);

    let body = serde_json::json!({
        "ts": ts,
        "cpu_count": cpu_count(),
        "cpu_used_pct": (cpu * 100.0).round() / 100.0,
        "mem_total_mib": mem_total / (1024 * 1024),
        "mem_used_mib": mem_used / (1024 * 1024),
        "mem_total": mem_total,
        "mem_used": mem_used,
        "mem_cache": mem_cache,
        "disk_used": disk_used,
        "disk_total": disk_total,
    });
    (
        [(axum::http::header::CACHE_CONTROL, "no-store")],
        axum::Json(body),
    )
        .into_response()
}

fn cpu_count() -> u64 {
    std::thread::available_parallelism()
        .map(|n| n.get() as u64)
        .unwrap_or(1)
}

/// Sample /proc/stat twice over a short window, like gopsutil's cpu.Percent
/// with a non-zero interval.
async fn cpu_used_pct() -> f64 {
    let Some(first) = read_cpu_totals() else {
        return 0.0;
    };
    tokio::time::sleep(std::time::Duration::from_millis(100)).await;
    let Some(second) = read_cpu_totals() else {
        return 0.0;
    };
    let total = second.0.saturating_sub(first.0);
    let idle = second.1.saturating_sub(first.1);
    if total == 0 {
        return 0.0;
    }
    ((total - idle) as f64 / total as f64) * 100.0
}

/// Returns (total_jiffies, idle_jiffies) from the aggregate cpu line.
fn read_cpu_totals() -> Option<(u64, u64)> {
    let stat = std::fs::read_to_string("/proc/stat").ok()?;
    let line = stat.lines().next()?;
    let fields: Vec<u64> = line
        .split_whitespace()
        .skip(1)
        .filter_map(|f| f.parse().ok())
        .collect();
    if fields.len() < 5 {
        return None;
    }
    let total: u64 = fields.iter().sum();
    let idle = fields[3] + fields.get(4).copied().unwrap_or(0); // idle + iowait
    Some((total, idle))
}

/// Returns (total, available, cached) in bytes from /proc/meminfo.
fn meminfo() -> (u64, u64, u64) {
    let mut total = 0u64;
    let mut available = 0u64;
    let mut cached = 0u64;
    if let Ok(content) = std::fs::read_to_string("/proc/meminfo") {
        for line in content.lines() {
            let mut parts = line.split_whitespace();
            let key = parts.next().unwrap_or("");
            let value: u64 = parts.next().and_then(|v| v.parse().ok()).unwrap_or(0);
            match key {
                "MemTotal:" => total = value * 1024,
                "MemAvailable:" => available = value * 1024,
                "Cached:" => cached = value * 1024,
                _ => {}
            }
        }
    }
    (total, available, cached)
}

fn disk_usage(path: &str) -> (u64, u64) {
    let c_path = std::ffi::CString::new(path).unwrap_or_default();
    let mut stat: libc::statvfs = unsafe { std::mem::zeroed() };
    if unsafe { libc::statvfs(c_path.as_ptr(), &mut stat) } != 0 {
        return (0, 0);
    }
    let block = stat.f_frsize as u64;
    let total = stat.f_blocks as u64 * block;
    let free = stat.f_bfree as u64 * block;
    (total, total.saturating_sub(free))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn proc_readers_do_not_panic() {
        let _ = read_cpu_totals();
        let (total, available, _) = meminfo();
        assert!(total > 0);
        assert!(available <= total);
        let (dt, du) = disk_usage("/");
        assert!(dt >= du);
    }
}
