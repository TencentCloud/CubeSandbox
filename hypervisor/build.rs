// Copyright © 2020 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

#[macro_use(crate_version)]
extern crate clap;

use std::process::Command;

fn main() {
    // Priority: CUBE_VERSION env > git describe > crate_version > fallback
    let version = if let Ok(v) = std::env::var("CUBE_VERSION") {
        v
    } else if let Ok(git_out) = Command::new("git").args(["describe", "--dirty"]).output() {
        if git_out.status.success() {
            if let Ok(s) = String::from_utf8(git_out.stdout) {
                s.trim().to_string()
            } else {
                "v".to_owned() + crate_version!()
            }
        } else {
            "v".to_owned() + crate_version!()
        }
    } else {
        "v".to_owned() + crate_version!()
    };

    let build_time =
        std::env::var("CUBE_BUILD_TIME").unwrap_or_else(|_| "unknown".to_string());

    // Embed build_time into BUILT_VERSION so clap's --version includes it.
    let built_version = format!("{} built at {}", version, build_time);

    println!("cargo:rustc-env=BUILT_VERSION={}", built_version);
    println!("cargo:rustc-env=SNAPSHOT_VERSION=1.0.0");
    println!("cargo:rerun-if-env-changed=CUBE_VERSION");
    println!("cargo:rerun-if-env-changed=CUBE_BUILD_TIME");
}
