// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

use std::os::unix::process::ExitStatusExt;
use std::process::Command;

use crash_recovery_worker::{Abort, Terminator};

const CHILD: &str = "CRASH_RECOVERY_ABORT_CHILD";

#[test]
fn abort_terminator_stops_the_process_with_sigabrt() {
    let status = Command::new(std::env::current_exe().expect("locate test executable"))
        .args(["--exact", "abort_child", "--nocapture"])
        .env(CHILD, "1")
        .status()
        .expect("run abort child");

    assert_eq!(status.signal(), Some(6));
}

#[test]
fn abort_child() {
    if std::env::var_os(CHILD).is_none() {
        return;
    }

    Abort.terminate();
}
