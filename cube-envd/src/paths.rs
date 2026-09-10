// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use crate::auth::LocalUser;
use std::path::{Path, PathBuf};
use thiserror::Error;

#[derive(Debug, Error, PartialEq, Eq)]
/// 描述用户主目录展开时的安全性错误。
pub enum PathError {
    #[error("cannot expand a different user's home directory")]
    OtherUserHome,
}

/// 将相对路径和 ~/ 路径限制在请求用户的主目录下解析。
///
/// 空路径与 `~/` 都解析为用户主目录本身，且不带尾部分隔符——上游用
/// `filepath.Join(home, "")` 表达同一语义，Join 会清理尾部斜杠。
pub fn resolve_path(path: impl AsRef<Path>, user: &LocalUser) -> Result<PathBuf, PathError> {
    let path = path.as_ref();
    let path = path.to_string_lossy();

    if path.is_empty() {
        return Ok(user.home.clone());
    }
    if let Some(rest) = path.strip_prefix("~/") {
        return Ok(if rest.is_empty() {
            user.home.clone()
        } else {
            user.home.join(rest)
        });
    }
    if path == "~" {
        return Ok(user.home.clone());
    }
    if path.starts_with('~') {
        return Err(PathError::OtherUserHome);
    }

    let path = Path::new(path.as_ref());
    if path.is_absolute() {
        Ok(path.into())
    } else {
        Ok(user.home.join(path))
    }
}
