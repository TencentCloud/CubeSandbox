// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

fn main() {
    let version = std::env::var("CUBE_VERSION").unwrap_or_else(|_| "0.0.0-dev".to_string());
    let commit = std::env::var("CUBE_COMMIT").unwrap_or_else(|_| "unknown".to_string());
    let build_time = std::env::var("CUBE_BUILD_TIME").unwrap_or_else(|_| "unknown".to_string());

    // Do NOT include the binary name — clap prepends it automatically.
    let version_full = format!("{} ({}) built at {}", version, commit, build_time);

    println!("cargo:rustc-env=CUBE_VERSION_FULL={}", version_full);
    println!("cargo:rustc-env=CUBE_VERSION={}", version);
    println!("cargo:rustc-env=CUBE_COMMIT={}", commit);
    println!("cargo:rustc-env=CUBE_BUILD_TIME={}", build_time);
    println!("cargo:rerun-if-env-changed=CUBE_VERSION");
    println!("cargo:rerun-if-env-changed=CUBE_COMMIT");
    println!("cargo:rerun-if-env-changed=CUBE_BUILD_TIME");
}
