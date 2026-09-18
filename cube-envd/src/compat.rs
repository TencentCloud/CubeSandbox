// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! Go 基线的错误词汇表：**只有数据表与纯函数**，不做 I/O、不持状态、不做决策。
//!
//! 为什么需要这一层：SDK 用户会 match 错误文本（对照套件也是逐字节比对），而 Go 与
//! Rust 在"errno 怎么说"这件事上系统性不同——
//!
//! - Go 的 `syscall.Errno.Error()` 是**小写**短语（`no such file or directory`），
//!   Rust 的 `std::io::Error` Display 走 `strerror`，首字母大写
//!   （`No such file or directory (os error 2)`）；
//! - Go 的错误串带 syscall 形状（`lstat <path>: no such file or directory`、
//!   `rename <src> <dst>: <errno>`），Rust 侧需要自己拼。
//!
//! 取值来源：Go `syscall/zerrors_linux_amd64.go`（go1.26）。表里没有的 errno 回落到
//! `std::io::Error` 的文本——它至少保留可读性，但会与基线不同，因此**新增用例时应优先
//! 补表**，而不是依赖回落。

use std::io::Error;
use std::path::Path;

/// Go 侧小写 errno 文本表（覆盖本 daemon 实际会碰到的 errno）。
const ERRNO_TEXTS: &[(i32, &str)] = &[
    (1, "operation not permitted"),
    (2, "no such file or directory"),
    (4, "interrupted system call"),
    (5, "input/output error"),
    (9, "bad file descriptor"),
    (11, "resource temporarily unavailable"),
    (12, "cannot allocate memory"),
    (13, "permission denied"),
    (16, "device or resource busy"),
    (17, "file exists"),
    (20, "not a directory"),
    (21, "is a directory"),
    (22, "invalid argument"),
    (24, "too many open files"),
    (27, "file too large"),
    (28, "no space left on device"),
    (30, "read-only file system"),
    (36, "file name too long"),
    (39, "directory not empty"),
    (40, "too many levels of symbolic links"),
    (122, "disk quota exceeded"),
];

/// 返回与 Go 基线一致的小写 errno 文本。
pub fn errno_text(error: &Error) -> String {
    if let Some(code) = error.raw_os_error() {
        if let Some((_, text)) = ERRNO_TEXTS.iter().find(|(number, _)| *number == code) {
            return (*text).to_owned();
        }
    }
    // 非 errno 错误（例如自定义 io::Error）：保留原始文本，便于定位。
    error.to_string()
}

/// 拼出 Go 的 syscall 形状：`<op> <path>: <errno>`，例如
/// `lstat /tmp/x: no such file or directory`。
pub fn go_syscall_error(op: &str, path: &Path, error: &Error) -> String {
    format!("{op} {}: {}", path.display(), errno_text(error))
}

/// 拼出 Go `os.Rename` 的形状：`rename <source> <destination>: <errno>`。
pub fn go_rename_error(source: &Path, destination: &Path, error: &Error) -> String {
    format!(
        "rename {} {}: {}",
        source.display(),
        destination.display(),
        errno_text(error)
    )
}
