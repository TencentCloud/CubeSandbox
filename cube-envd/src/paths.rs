use std::path::{Path, PathBuf};
use thiserror::Error;
use crate::auth::LocalUser;

#[derive(Debug, Error, PartialEq, Eq)]
/// 描述用户主目录展开时的安全性错误。
pub enum PathError {
    #[error("cannot expand a different user's home directory")]
    OtherUserHome,
}

/// 将相对路径和 ~/ 路径限制在请求用户的主目录下解析。
pub fn resolve_path(path: impl AsRef<Path>, user: &LocalUser) -> Result<PathBuf, PathError> {
    let path = path.as_ref();
    let path = path.to_string_lossy();

    if let Some(rest) = path.strip_prefix("~/") {
        return Ok(user.home.join(rest));
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
