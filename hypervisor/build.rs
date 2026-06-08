// Copyright © 2020 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

#[macro_use(crate_version)]
extern crate clap;

use std::process::Command;

fn main() {
    // Priority: CUBE_VERSION env > git describe > crate_version > fallback
    let version = std::env::var("CUBE_VERSION")
        .ok()
        .or_else(|| git_output(&["describe", "--dirty"]))
        .unwrap_or_else(default_version);

    let build_time = std::env::var("CUBE_BUILD_TIME").unwrap_or_else(|_| "unknown".to_string());
    let commit = std::env::var("CUBE_COMMIT")
        .ok()
        .or_else(|| git_output(&["rev-parse", "HEAD"]).filter(|s| !s.is_empty()))
        .unwrap_or_else(|| "unknown".to_string());

    // Embed metadata into BUILT_VERSION so clap's --version includes it.
    let built_version = format!("{} ({}) built at {}", version, commit, build_time);

    println!("cargo:rustc-env=BUILT_VERSION={}", built_version);
    println!("cargo:rustc-env=SNAPSHOT_VERSION=1.0.0");
    println!("cargo:rerun-if-env-changed=CUBE_VERSION");
    println!("cargo:rerun-if-env-changed=CUBE_COMMIT");
    println!("cargo:rerun-if-env-changed=CUBE_BUILD_TIME");
}

fn git_output(args: &[&str]) -> Option<String> {
    let output = Command::new("git").args(args).output().ok()?;
    if !output.status.success() {
        return None;
    }

    String::from_utf8(output.stdout)
        .ok()
        .map(|stdout| stdout.trim().to_string())
}

fn default_version() -> String {
    "v".to_owned() + crate_version!()
}
