use axum::response::Json;
use serde::Serialize;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

/// Response shape of `GET /metrics`, matching upstream envd's `Metrics`.
#[derive(Debug, Serialize)]
pub(crate) struct Metrics {
    ts: i64,
    cpu_count: u64,
    cpu_used_pct: f64,
    mem_total: u64,
    mem_used: u64,
    mem_cache: u64,
    mem_total_mib: u64,
    mem_used_mib: u64,
    disk_used: u64,
    disk_total: u64,
}

pub async fn metrics() -> Json<Metrics> {
    let (mem_total, mem_used, mem_cache) = memory();
    let (disk_used, disk_total) = disk();
    let first = std::fs::read_to_string("/proc/stat")
        .ok()
        .and_then(|contents| parse_cpu_line(&contents));
    tokio::time::sleep(Duration::from_millis(200)).await;
    let second = std::fs::read_to_string("/proc/stat")
        .ok()
        .and_then(|contents| parse_cpu_line(&contents));

    Json(Metrics {
        ts: now_seconds(),
        cpu_count: cpu_count(),
        cpu_used_pct: cpu_percent(first, second),
        mem_total,
        mem_used,
        mem_cache,
        mem_total_mib: mem_total / 1024 / 1024,
        mem_used_mib: mem_used / 1024 / 1024,
        disk_used,
        disk_total,
    })
}

fn now_seconds() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|duration| duration.as_secs() as i64)
        .unwrap_or(0)
}

fn cpu_count() -> u64 {
    std::thread::available_parallelism()
        .map(|count| count.get() as u64)
        .unwrap_or(1)
}

/// `(mem_total, mem_used, mem_cache)` in bytes from `/proc/meminfo`.
fn memory() -> (u64, u64, u64) {
    match std::fs::read_to_string("/proc/meminfo") {
        Ok(contents) => parse_meminfo(&contents),
        Err(_) => (0, 0, 0),
    }
}

fn parse_meminfo(contents: &str) -> (u64, u64, u64) {
    let mut total_kib = 0u64;
    let mut available_kib = 0u64;
    let mut cache_kib = 0u64;
    for line in contents.lines() {
        let mut fields = line.split_whitespace();
        let Some(key) = fields.next() else { continue };
        let Some(value) = fields.next().and_then(|value| value.parse::<u64>().ok()) else {
            continue;
        };
        match key {
            "MemTotal:" => total_kib = value,
            "MemAvailable:" => available_kib = value,
            "Cached:" => cache_kib += value,
            "SReclaimable:" => cache_kib += value,
            _ => {}
        }
    }
    let total = total_kib.saturating_mul(1024);
    let used = total_kib.saturating_sub(available_kib).saturating_mul(1024);
    let cache = cache_kib.saturating_mul(1024);
    (total, used, cache)
}

/// `(busy_jiffies, total_jiffies)` from the aggregate `cpu ` line.
fn parse_cpu_line(contents: &str) -> Option<(u64, u64)> {
    let line = contents.lines().find(|line| line.starts_with("cpu "))?;
    let values: Vec<u64> = line
        .split_whitespace()
        .skip(1)
        .filter_map(|value| value.parse().ok())
        .collect();
    if values.len() < 5 {
        return None;
    }
    let total: u64 = values.iter().sum();
    let idle = values[3].saturating_add(values[4]);
    Some((total.saturating_sub(idle), total))
}

fn cpu_percent(first: Option<(u64, u64)>, second: Option<(u64, u64)>) -> f64 {
    match (first, second) {
        (Some((busy1, total1)), Some((busy2, total2))) if total2 > total1 => {
            let busy = busy2.saturating_sub(busy1) as f64;
            let total = (total2 - total1) as f64;
            ((busy / total) * 100.0 * 100.0).round() / 100.0
        }
        _ => 0.0,
    }
}

/// `(used, total)` bytes of the root filesystem.
fn disk() -> (u64, u64) {
    match nix::sys::statvfs::statvfs("/") {
        Ok(stat) => {
            let unit = to_u64(stat.fragment_size());
            let total = to_u64(stat.blocks()).saturating_mul(unit);
            let free = to_u64(stat.blocks_free()).saturating_mul(unit);
            (total.saturating_sub(free), total)
        }
        Err(_) => (0, 0),
    }
}

fn to_u64<T: TryInto<u64>>(value: T) -> u64 {
    value.try_into().ok().unwrap_or(0)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn meminfo_parser_reads_total_available_and_cache() {
        let contents = "MemTotal:       1000000 kB\nMemFree:  1 kB\nMemAvailable:    400000 kB\nCached:           50000 kB\nSReclaimable:     10000 kB\n";
        let (total, used, cache) = parse_meminfo(contents);
        assert_eq!(total, 1_000_000 * 1024);
        assert_eq!(used, 600_000 * 1024);
        assert_eq!(cache, 60_000 * 1024);
    }

    #[test]
    fn meminfo_parser_tolerates_missing_fields() {
        assert_eq!(parse_meminfo("garbage\n"), (0, 0, 0));
    }

    #[test]
    fn cpu_line_parser_reads_busy_and_total() {
        let contents = "cpu  100 0 50 1000 20 0 0 0 0 0\ncpu0 1 2 3 4\n";
        assert_eq!(parse_cpu_line(contents), Some((150, 1170)));
    }

    #[test]
    fn cpu_line_parser_rejects_short_lines() {
        assert_eq!(parse_cpu_line("cpu 1 2\n"), None);
        assert_eq!(parse_cpu_line("notcpu\n"), None);
    }

    #[test]
    fn cpu_percent_uses_deltas() {
        assert_eq!(cpu_percent(Some((100, 1000)), Some((150, 1200))), 25.0);
        assert_eq!(cpu_percent(None, Some((1, 2))), 0.0);
        assert_eq!(cpu_percent(Some((1, 2)), Some((1, 2))), 0.0);
    }
}
