// SPDX-License-Identifier: Apache-2.0

#[test]
fn signed_files_and_compose() {
    let status = std::process::Command::new("python3")
        .args([
            "-B",
            concat!(env!("CARGO_MANIFEST_DIR"), "/tests/file_transfer.py"),
        ])
        .env("ENVD_TEST_BINARY", env!("CARGO_BIN_EXE_cube-envd"))
        .status()
        .expect("run public file transfer contracts");
    assert!(status.success());
}
