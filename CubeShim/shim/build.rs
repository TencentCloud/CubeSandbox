// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use std::process::Command;

const ENV_BUILD_ENV_K: &str = "BUILD_ENV";
const ENV_BUILD_ENV_V: &str = "zhiyan";

fn main() {
    // ── Version (priority: env var > git tag > fallback) ──
    let version = std::env::var("CUBE_VERSION").unwrap_or_else(|_| "0.0.0-dev".to_string());

    // ── Commit (priority: env var > git rev-parse > fallback) ──
    let commit = std::env::var("CUBE_COMMIT").unwrap_or_else(|_| {
        Command::new("git")
            .args(["rev-parse", "HEAD"])
            .output()
            .ok()
            .and_then(|o| String::from_utf8(o.stdout).ok())
            .map(|s| s.trim().to_string())
            .unwrap_or_else(|| "unknown".to_string())
    });

    // ── Build time (priority: env var > fallback) ──
    let build_time = std::env::var("CUBE_BUILD_TIME").unwrap_or_else(|_| "unknown".to_string());

    // ── Short commit + dirty check (backward compatible GIT_COMMIT_INFO) ──
    let short_commit: String = commit.chars().take(8).collect();
    let zhiyan = std::env::var(ENV_BUILD_ENV_K)
        .map(|v| v == ENV_BUILD_ENV_V)
        .unwrap_or(false);

    let dirty = Command::new("git")
        .args(["status", "--porcelain"])
        .output()
        .ok()
        .and_then(|o| String::from_utf8(o.stdout).ok())
        .map(|s| {
            let cleaned = s.replace([' ', '\t', '\n'], "");
            !cleaned.is_empty()
        })
        .unwrap_or(false);

    let commit_info = if dirty && !zhiyan {
        format!("{}--dirty", short_commit)
    } else {
        short_commit
    };

    // ── Emit env vars for the crate ──
    println!("cargo:rustc-env=CUBE_VERSION={}", version);
    println!("cargo:rustc-env=CUBE_COMMIT={}", commit);
    println!("cargo:rustc-env=CUBE_BUILD_TIME={}", build_time);
    println!("cargo:rustc-env=GIT_COMMIT_INFO={}", commit_info);
    println!("cargo:rerun-if-env-changed=CUBE_VERSION");
    println!("cargo:rerun-if-env-changed=CUBE_COMMIT");
    println!("cargo:rerun-if-env-changed=CUBE_BUILD_TIME");
    println!("cargo:rerun-if-changed=build.rs");
}
