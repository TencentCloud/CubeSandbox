// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Startup limits that a deployment may tune: the blocking-pool size, and the
//! share of it the `/files` data plane is allowed to pin.
//!
//! They live below `app/` because two layers need the same number: `main.rs`
//! sizes the tokio runtime with it, and the filesystem data plane derives its
//! prefetch budget from it (the layer rule forbids `filesystem` reaching into
//! `app/`). The env-var shape copies `process::cgroup`: a `CUBE_ENVD_*` name,
//! parsed once, an invalid value warns and falls back to the default instead of
//! failing startup, and the adjudication is a pure function so it is testable
//! without touching the process environment.

use std::sync::OnceLock;

/// Blocking-pool thread cap (`max_blocking_threads`). 64 keeps the pool's
/// worst-case touched RSS (~13 KiB/thread) inside this daemon's budget; over the
/// cap, work queues instead of erroring. A *deployment* property, so it is
/// configurable.
pub const DEFAULT_BLOCKING_THREADS: usize = 64;
const BLOCKING_THREADS_ENV: &str = "CUBE_ENVD_BLOCKING_THREADS";
/// Below this the pool cannot serve the daemon's own concurrent work; above it
/// the idle-thread RSS stops being defensible inside a guest.
const BLOCKING_THREADS_MIN: usize = 4;
const BLOCKING_THREADS_MAX: usize = 256;

/// `/files` may pin at most `pool / this` blocking threads: downloads are the
/// only user that holds a pool thread for a whole client stall, so they get a
/// quarter of the pool. Derived, never configured alone — a budget above the
/// pool would reintroduce the starvation it exists to bound.
const DOWNLOAD_BLOCKING_DIVISOR: usize = 4;

/// `/files` may have at most `pool / this` bodies *buffering ahead* (~4.5 MiB
/// each while a client stalls); the rest stream 256 KiB slices without
/// read-ahead. The thread budget bounds threads, this one bounds memory.
const DOWNLOAD_BUFFERED_DIVISOR: usize = 2;

/// How many *large* downloads may be in flight at once, globally. A request over
/// the cap is refused (`503`), never queued — a stalled client holding a slot
/// must not put every later download behind it. The default is twice the pool;
/// an explicit value is clamped to `[floor, ceiling]`, where the floor is what
/// the pipeline itself needs (blocking producers plus buffered bodies).
const DOWNLOAD_MAX_BODIES_ENV: &str = "CUBE_ENVD_DOWNLOAD_MAX_BODIES";
const DOWNLOAD_MAX_BODIES_CEILING: usize = 1024;

static BLOCKING_THREADS: OnceLock<usize> = OnceLock::new();
static MAX_BODIES: OnceLock<usize> = OnceLock::new();

/// Adjudicate the command-line flags once, before the runtime is built. The
/// flags are the entrypoint's documented surface (`ENVD_EXTRA_ARGS` forwards
/// flags, and only flags cube-envd declares), so they win over the equivalent
/// environment variables; either way the value is clamped and logged rather
/// than refused.
///
/// Called by `main.rs` exactly once; every accessor falls back to the
/// environment and then to its default when it has not been called (unit
/// tests, and any caller that runs before startup).
pub fn configure(blocking_threads_flag: Option<usize>, max_bodies_flag: Option<usize>) {
    if let Some(flag) = blocking_threads_flag {
        let _ = BLOCKING_THREADS.set(clamp_blocking_threads(flag, "flag -blocking-threads"));
    }
    if let Some(flag) = max_bodies_flag {
        // The cap's floor depends on the pool, so resolve the pool first (the
        // flag above, else the environment, else the default).
        let pool = blocking_threads();
        let _ = MAX_BODIES.set(effective_max_bodies(Some(flag), None, pool));
    }
}

/// Blocking-pool cap: `CUBE_ENVD_BLOCKING_THREADS`, else the default.
pub fn blocking_threads() -> usize {
    *BLOCKING_THREADS
        .get_or_init(|| effective_blocking_threads(None, env_var(BLOCKING_THREADS_ENV).as_deref()))
}

/// How many `/files` bodies may run the blocking (pool-thread) producer.
pub fn download_blocking_producers() -> usize {
    download_blocking_producers_at(blocking_threads())
}

fn download_blocking_producers_at(pool: usize) -> usize {
    (pool / DOWNLOAD_BLOCKING_DIVISOR).max(1)
}

/// How many `/files` bodies may buffer ahead at all (1 MiB slices); the rest
/// stream 256 KiB slices without read-ahead.
pub fn download_buffered_bodies() -> usize {
    download_buffered_bodies_at(blocking_threads())
}

/// Bytes of read buffers the download path may keep recycled: one buffer per
/// buffered body, capped at [`DOWNLOAD_POOL_BODIES_MAX`] bodies. So it follows
/// `-blocking-threads` / `CUBE_ENVD_BLOCKING_THREADS` down (4 MiB at
/// `-blocking-threads 8`, 2 MiB at the floor of 4) and stops at 32 MiB — the
/// default — however large the pool is configured. A body holds up to
/// `READ_AHEAD + 2` buffers, and keeping one per buffered body warm is what
/// removes the per-request page faults; above ~8..16 MiB the curve flattens.
pub fn download_pool_bytes(chunk_bytes: usize) -> usize {
    download_pool_bytes_at(blocking_threads(), chunk_bytes)
}

fn download_pool_bytes_at(pool: usize, chunk_bytes: usize) -> usize {
    download_buffered_bodies_at(pool).min(DOWNLOAD_POOL_BODIES_MAX) * chunk_bytes
}

/// Hard ceiling of the pool budget, in buffered bodies: 32 MiB. The pool stays
/// a bounded working set even when the pool itself is configured for hundreds
/// of threads.
const DOWNLOAD_POOL_BODIES_MAX: usize = 32;

/// Global cap on concurrent large `/files` downloads (see the env doc above).
pub fn download_max_bodies() -> usize {
    *MAX_BODIES.get_or_init(|| {
        let pool = blocking_threads();
        effective_max_bodies(None, env_var(DOWNLOAD_MAX_BODIES_ENV).as_deref(), pool)
    })
}

fn env_var(name: &str) -> Option<String> {
    match std::env::var(name) {
        Ok(raw) => Some(raw),
        Err(std::env::VarError::NotPresent) => None,
        Err(e) => {
            tracing::warn!("limits: cannot read {name}: {e}");
            None
        }
    }
}

/// The flag wins over the environment; both go through the same clamps.
fn effective_blocking_threads(flag: Option<usize>, env: Option<&str>) -> usize {
    match flag {
        Some(n) => clamp_blocking_threads(n, "flag -blocking-threads"),
        None => blocking_threads_from(env),
    }
}

fn clamp_blocking_threads(n: usize, source: &str) -> usize {
    if (BLOCKING_THREADS_MIN..=BLOCKING_THREADS_MAX).contains(&n) {
        n
    } else {
        let clamped = n.clamp(BLOCKING_THREADS_MIN, BLOCKING_THREADS_MAX);
        tracing::warn!("limits: {source}={n} is out of range; using {clamped}");
        clamped
    }
}

/// The flag wins over the environment; the floor and ceiling apply to both.
fn effective_max_bodies(flag: Option<usize>, env: Option<&str>, pool: usize) -> usize {
    match flag {
        Some(n) => download_max_bodies_from(Some(&n.to_string()), pool),
        None => download_max_bodies_from(env, pool),
    }
}

fn download_buffered_bodies_at(pool: usize) -> usize {
    (pool / DOWNLOAD_BUFFERED_DIVISOR).max(1)
}

fn download_max_bodies_from(raw: Option<&str>, pool: usize) -> usize {
    let floor = download_blocking_producers_at(pool) + download_buffered_bodies_at(pool);
    let default = (pool * 2).max(floor);
    let Some(raw) = raw else {
        return default;
    };
    match raw.parse::<usize>() {
        Ok(0) | Err(_) => {
            tracing::warn!(
                "limits: ignoring invalid download body cap {raw:?}; \
                 expected a positive body count"
            );
            default
        }
        Ok(n) if n < floor => {
            tracing::warn!(
                "limits: download body cap {n} is below what the pipeline needs; using {floor}"
            );
            floor
        }
        Ok(n) if n > DOWNLOAD_MAX_BODIES_CEILING => {
            tracing::warn!(
                "limits: clamping download body cap {n} to {DOWNLOAD_MAX_BODIES_CEILING}"
            );
            DOWNLOAD_MAX_BODIES_CEILING
        }
        Ok(n) => n,
    }
}

/// Adjudicate a configured value: unset or unparsable falls back to the
/// default, and anything outside the sane range is clamped rather than
/// refused (an in-guest daemon that refuses to start over a typo is worse than
/// one that runs with a warned-about value).
fn blocking_threads_from(raw: Option<&str>) -> usize {
    let Some(raw) = raw else {
        return DEFAULT_BLOCKING_THREADS;
    };
    match raw.parse::<usize>() {
        Ok(0) | Err(_) => {
            tracing::warn!(
                "limits: ignoring invalid {BLOCKING_THREADS_ENV}={raw:?}; \
                 expected a positive thread count"
            );
            DEFAULT_BLOCKING_THREADS
        }
        Ok(n) if !(BLOCKING_THREADS_MIN..=BLOCKING_THREADS_MAX).contains(&n) => {
            let clamped = n.clamp(BLOCKING_THREADS_MIN, BLOCKING_THREADS_MAX);
            tracing::warn!("limits: clamping {BLOCKING_THREADS_ENV}={n} to {clamped}");
            clamped
        }
        Ok(n) => n,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn an_unset_or_invalid_value_falls_back_to_the_default() {
        assert_eq!(blocking_threads_from(None), DEFAULT_BLOCKING_THREADS);
        for raw in ["", "abc", "0", "-1", "64.5", " 64"] {
            assert_eq!(
                blocking_threads_from(Some(raw)),
                DEFAULT_BLOCKING_THREADS,
                "{raw:?} must fall back, not fail startup"
            );
        }
    }

    #[test]
    fn a_valid_value_is_used_and_an_extreme_one_is_clamped() {
        assert_eq!(blocking_threads_from(Some("8")), 8);
        assert_eq!(blocking_threads_from(Some("256")), 256);
        assert_eq!(blocking_threads_from(Some("1")), BLOCKING_THREADS_MIN);
        assert_eq!(blocking_threads_from(Some("100000")), BLOCKING_THREADS_MAX);
    }

    /// Both budgets are derived, so lowering the pool lowers them too: that is
    /// the invariant that keeps downloads from pinning the whole pool or from
    /// buffering without bound.
    /// The download cap never drops below the pipeline's own concurrency and
    /// never exceeds the ceiling, whatever the environment says.
    #[test]
    fn the_download_cap_is_floored_by_the_pipeline_and_capped() {
        let floor = download_blocking_producers_at(DEFAULT_BLOCKING_THREADS)
            + download_buffered_bodies_at(DEFAULT_BLOCKING_THREADS);
        assert_eq!(
            download_max_bodies_from(None, DEFAULT_BLOCKING_THREADS),
            128
        );
        assert_eq!(
            download_max_bodies_from(Some("256"), DEFAULT_BLOCKING_THREADS),
            256
        );
        assert_eq!(
            download_max_bodies_from(Some("8"), DEFAULT_BLOCKING_THREADS),
            floor,
            "a value below the floor is raised to it"
        );
        assert_eq!(
            download_max_bodies_from(Some("100000"), DEFAULT_BLOCKING_THREADS),
            DOWNLOAD_MAX_BODIES_CEILING
        );
        for raw in ["", "abc", "0", "-4"] {
            assert_eq!(
                download_max_bodies_from(Some(raw), DEFAULT_BLOCKING_THREADS),
                128,
                "{raw:?} must fall back to the default"
            );
        }
        // Smaller pools scale the whole thing down, floor included.
        assert_eq!(download_max_bodies_from(None, 8), 16);
        assert_eq!(download_max_bodies_from(Some("1"), 8), 6);
    }

    /// A flag beats the environment, and both go through the same clamps.
    #[test]
    fn a_flag_wins_over_the_environment() {
        assert_eq!(effective_blocking_threads(Some(8), Some("32")), 8);
        assert_eq!(effective_blocking_threads(None, Some("32")), 32);
        assert_eq!(
            effective_blocking_threads(Some(1), None),
            BLOCKING_THREADS_MIN
        );
        assert_eq!(
            effective_blocking_threads(Some(4096), None),
            BLOCKING_THREADS_MAX
        );

        assert_eq!(effective_max_bodies(Some(256), Some("64"), 64), 256);
        assert_eq!(effective_max_bodies(None, Some("64"), 64), 64);
        assert_eq!(
            effective_max_bodies(Some(1), None, 64),
            48,
            "floor applies to flags too"
        );
        assert_eq!(
            effective_max_bodies(Some(9999), None, 64),
            DOWNLOAD_MAX_BODIES_CEILING
        );
    }

    #[test]
    fn the_download_budgets_follow_the_pool_and_never_reach_zero() {
        assert_eq!(download_blocking_producers_at(DEFAULT_BLOCKING_THREADS), 16);
        assert_eq!(download_buffered_bodies_at(DEFAULT_BLOCKING_THREADS), 32);
        assert_eq!(download_blocking_producers_at(8), 2);
        assert_eq!(download_buffered_bodies_at(8), 4);
        assert_eq!(download_blocking_producers_at(1), 1);
        assert_eq!(download_buffered_bodies_at(1), 1);
        // The blocking sub-tier can never exceed the buffered one at any pool
        // size the parser admits.
        for pool in (BLOCKING_THREADS_MIN..=BLOCKING_THREADS_MAX).step_by(7) {
            assert!(download_blocking_producers_at(pool) <= download_buffered_bodies_at(pool));
        }
    }

    #[test]
    fn the_pool_budget_scales_with_the_pool_and_the_chunk() {
        const MIB: usize = 1024 * 1024;
        // One read buffer per buffered body: 32 MiB at the default pool, and it
        // follows the knob both ways, so a small deployment does not hold a
        // default-sized cache.
        assert_eq!(download_pool_bytes_at(64, MIB), 32 * MIB);
        assert_eq!(download_pool_bytes_at(8, MIB), 4 * MIB);
        assert_eq!(download_pool_bytes_at(4, MIB), 2 * MIB);
        // It scales down with the knob and never past the 32 MiB ceiling, at any
        // chunk size the body pipeline may hand it.
        for pool in [4usize, 8, 16, 64, 128, 256, 1024] {
            for chunk in [256 * 1024usize, MIB] {
                let bodies = download_buffered_bodies_at(pool).min(DOWNLOAD_POOL_BODIES_MAX);
                assert_eq!(
                    download_pool_bytes_at(pool, chunk),
                    bodies * chunk,
                    "pool = {pool}, chunk = {chunk}"
                );
                assert!(
                    download_pool_bytes_at(pool, chunk) <= 32 * MIB,
                    "pool = {pool}, chunk = {chunk}"
                );
            }
        }
        assert_eq!(download_pool_bytes_at(256, MIB), 32 * MIB);
        assert_eq!(download_pool_bytes_at(256, 256 * 1024), 8 * MIB);
    }
}
