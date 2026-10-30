// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

fn main() {
    println!("cargo:rerun-if-changed=build.rs");
    println!("cargo:rerun-if-changed=../.git/HEAD");
    println!("cargo:rerun-if-changed=../.git/logs/HEAD");
    println!("cargo:rerun-if-env-changed=CUBE_ENVD_COMMIT");
    println!("cargo:rerun-if-changed=proto/process.proto");
    println!("cargo:rerun-if-changed=proto/filesystem.proto");
    println!("cargo:rerun-if-changed=proto/google/protobuf/timestamp.proto");

    println!("cargo:rerun-if-env-changed=CARGO_FEATURE_TEST_SUPPORT");

    println!(
        "cargo:rustc-env=CUBE_ENVD_VERSION={}",
        env!("CARGO_PKG_VERSION")
    );
    let revision = std::env::var("CUBE_ENVD_COMMIT").unwrap_or_else(|_| {
        let output = std::process::Command::new("git")
            .args(["--git-dir=../.git", "--work-tree=..", "rev-parse", "HEAD"])
            .current_dir(env!("CARGO_MANIFEST_DIR"))
            .output()
            .expect("run git rev-parse HEAD");
        assert!(output.status.success(), "git rev-parse HEAD failed");
        String::from_utf8(output.stdout)
            .expect("Git revision is UTF-8")
            .trim()
            .to_owned()
    });
    assert!(
        revision.len() == 40 && revision.bytes().all(|byte| byte.is_ascii_hexdigit()),
        "CUBE_ENVD_COMMIT must be a full Git SHA"
    );
    println!("cargo:rustc-env=CUBE_ENVD_COMMIT={revision}");
    println!(
        "cargo:rustc-env=CUBE_ENVD_BUILD_TARGET={}",
        std::env::var("TARGET").expect("Cargo build target")
    );
    println!(
        "cargo:rustc-env=CUBE_ENVD_TARGET_OS={}",
        std::env::var("CARGO_CFG_TARGET_OS").expect("Cargo target OS")
    );
    let target_arch = std::env::var("CARGO_CFG_TARGET_ARCH").expect("Cargo target architecture");
    let artifact_arch = if target_arch == "x86_64" {
        "amd64"
    } else {
        target_arch.as_str()
    };
    println!("cargo:rustc-env=CUBE_ENVD_TARGET_ARCHITECTURE={artifact_arch}");

    let protoc = protoc_bin_vendored::protoc_bin_path().expect("vendored protoc unavailable");
    let version = std::process::Command::new(&protoc)
        .arg("--version")
        .output()
        .expect("run vendored protoc");
    assert!(version.status.success(), "vendored protoc --version failed");
    let version = String::from_utf8(version.stdout).expect("protoc version is UTF-8");
    let version = version.trim();
    assert_eq!(version, "libprotoc 28.2", "unexpected vendored protoc");
    println!("cargo:rustc-env=CUBE_ENVD_PROTOC_VERSION={version}");
    std::env::set_var("PROTOC", protoc);

    let mut proto_files = vec!["proto/process.proto", "proto/filesystem.proto"];
    if std::env::var_os("CARGO_FEATURE_TEST_SUPPORT").is_some() {
        println!("cargo:rerun-if-changed=proto/conformance/echo.proto");
        proto_files.push("proto/conformance/echo.proto");
    }

    connectrpc_build::Config::new()
        .files(&proto_files)
        .includes(&["proto/"])
        .include_file("_connectrpc.rs")
        .compile()
        .expect("connectrpc code generation failed");
}
