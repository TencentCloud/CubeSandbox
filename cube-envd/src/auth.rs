use std::{ffi::CString, path::PathBuf};

use axum::{http::header::AUTHORIZATION, http::HeaderMap};
use base64::{engine::general_purpose::STANDARD, Engine};
use nix::unistd::{getgrouplist, Gid, User};
use thiserror::Error;

#[derive(Debug, Clone, PartialEq, Eq)]
/// 表示从宿主 passwd 数据库解析出的本地执行用户。
pub struct LocalUser {
    /// 用户名。
    pub name: String,
    /// 用户 ID。
    pub uid: u32,
    /// 主组 ID。
    pub gid: u32,
    /// 用户所属的全部组 ID。
    pub groups: Vec<u32>,
    /// 用户主目录。
    pub home: PathBuf,
}

#[derive(Debug, Error)]
/// 描述 Basic 认证或本地账户解析失败的原因。
pub enum AuthError {
    #[error("invalid Basic authorization header")]
    InvalidAuthorization,
    #[error("local user {0:?} was not found")]
    UnknownUser(String),
    #[error("could not resolve local user: {0}")]
    System(#[from] nix::Error),
}

/// 从 Basic Authorization 请求头提取用户名，缺失时默认使用 root。
pub fn basic_username(headers: &HeaderMap) -> Result<String, AuthError> {
    let Some(value) = headers.get(AUTHORIZATION) else {
        return Ok("root".into());
    };
    let value = value
        .to_str()
        .map_err(|_| AuthError::InvalidAuthorization)?;
    let encoded = value
        .strip_prefix("Basic ")
        .ok_or(AuthError::InvalidAuthorization)?;
    let decoded = STANDARD
        .decode(encoded)
        .map_err(|_| AuthError::InvalidAuthorization)?;
    let decoded = std::str::from_utf8(&decoded).map_err(|_| AuthError::InvalidAuthorization)?;
    let (username, _) = decoded
        .split_once(':')
        .ok_or(AuthError::InvalidAuthorization)?;
    if username.is_empty() {
        return Err(AuthError::InvalidAuthorization);
    }

    Ok(username.into())
}

/// 从系统用户数据库加载用户及其补充组信息。
pub fn resolve_user(username: &str) -> Result<LocalUser, AuthError> {
    let user = User::from_name(username)?.ok_or_else(|| AuthError::UnknownUser(username.into()))?;
    let name = CString::new(user.name.clone()).map_err(|_| AuthError::InvalidAuthorization)?;
    let groups = getgrouplist(&name, user.gid)?
        .into_iter()
        .map(|gid| gid.as_raw())
        .collect();

    Ok(LocalUser {
        name: user.name,
        uid: user.uid.as_raw(),
        gid: user.gid.as_raw(),
        groups,
        home: user.dir,
    })
}

/// 根据请求认证头解析实际执行用户。
pub fn request_user(headers: &HeaderMap) -> Result<LocalUser, AuthError> {
    resolve_user(&basic_username(headers)?)
}

/// 解析默认的 root 执行用户。
pub fn root_user() -> Result<LocalUser, AuthError> {
    resolve_user("root")
}

/// 将本地用户的主组 ID 转换为 nix 所需的 Gid 类型。
pub fn primary_gid(user: &LocalUser) -> Gid {
    Gid::from_raw(user.gid)
}
