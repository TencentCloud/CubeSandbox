// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use std::path::Path;

use crate::{
    compat,
    connect::{Code, RpcError},
};

/// stat 类操作失败：基线的文案是 `file not found: lstat <path>: <errno>`
/// （`internal/services/filesystem/utils.go`），缺失与其它错误共用同一个 syscall 形状。
pub(super) fn filesystem_error(path: &Path, error: std::io::Error) -> RpcError {
    let detail = compat::go_syscall_error("lstat", path, &error);
    if error.kind() == std::io::ErrorKind::NotFound {
        RpcError::new(Code::NotFound, format!("file not found: {detail}"))
    } else {
        RpcError::new(Code::Internal, format!("error getting file info: {detail}"))
    }
}

/// ListDir 的根路径不存在：基线的文案是 `path not found: lstat <path>: <errno>`
/// （`internal/services/filesystem/dir.go`），与 Stat 的 `file not found:` 前缀不同。
pub(super) fn path_not_found(path: &Path, error: std::io::Error) -> RpcError {
    RpcError::new(
        Code::NotFound,
        format!(
            "path not found: {}",
            compat::go_syscall_error("lstat", path, &error)
        ),
    )
}

/// 路径本身不是目录（ListDir 等）：基线的文案是 `path is not a directory: <path>`。
pub(super) fn not_a_directory(path: &Path) -> RpcError {
    RpcError::invalid_argument(format!("path is not a directory: {}", path.display()))
}
