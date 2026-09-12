// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use std::{
    hash::{BuildHasher, Hasher},
    os::unix::fs::PermissionsExt,
    path::{Path, PathBuf},
    sync::atomic::{AtomicU64, Ordering},
};

use axum::{
    body::Body,
    extract::{Query, Request},
    http::{header::CONTENT_TYPE, StatusCode},
    response::{IntoResponse, Response},
    Json,
};
use futures_util::StreamExt;
use serde::{Deserialize, Serialize};
use tokio::{
    fs::{self, File, OpenOptions},
    io::AsyncWriteExt,
    task,
};
use tokio_util::io::ReaderStream;

use crate::{
    auth::{resolve_user, AuthError, LocalUser},
    connect::{Code, RpcError},
    paths::resolve_path,
};

/// 为同一进程中的上传临时文件提供递增序号。
static TEMP_SEQUENCE: AtomicU64 = AtomicU64::new(0);

#[derive(Debug, Deserialize)]
/// 表示 /files 查询参数中的路径和可选执行用户。
pub struct FilesQuery {
    /// 要下载或上传的目标路径。
    pub path: Option<String>,
    /// 可选的本地执行用户名，缺失时使用 root。
    pub username: Option<String>,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
/// 表示上传成功后返回的文件条目。
struct UploadEntry {
    /// 文件名。
    name: String,
    /// 文件的完整路径。
    path: String,
    /// 固定为 file 的条目类型。
    r#type: &'static str,
}

/// 流式下载指定用户可访问的常规文件。
pub async fn download(Query(query): Query<FilesQuery>) -> Result<Response, RpcError> {
    let path = resolve_query_path(&query)?;
    let metadata = fs::metadata(&path)
        .await
        .map_err(|error| file_error(&path, error))?;
    if !metadata.is_file() {
        return Err(RpcError::invalid_argument(format!(
            "path {} is not a regular file",
            path.display()
        )));
    }

    let file = File::open(&path)
        .await
        .map_err(|error| file_error(&path, error))?;
    Ok((StatusCode::OK, Body::from_stream(ReaderStream::new(file))).into_response())
}

/// 接收原始或 multipart 请求体，并以原子替换方式上传文件。
pub async fn upload(request: Request) -> Result<Response, RpcError> {
    let query = serde_urlencoded::from_str(request.uri().query().unwrap_or_default())
        .map_err(|error| RpcError::invalid_argument(format!("invalid file query: {error}")))?;
    let content_type = request
        .headers()
        .get(CONTENT_TYPE)
        .and_then(|value| value.to_str().ok())
        .map(str::to_owned);
    let user = resolve_query_user(&query)?;
    let (_, body) = request.into_parts();

    match content_type.as_deref() {
        Some("application/octet-stream") => {
            let path = resolve_query_path(&query)?;
            write_atomically(&path, &user, body).await?;
            Ok(Json(vec![upload_entry(&path)]).into_response())
        }
        Some(value) if value.starts_with("multipart/form-data") => {
            upload_multipart(&query, &user, value, body).await
        }
        _ => Err(RpcError::invalid_argument(
            "POST /files requires application/octet-stream or multipart/form-data",
        )),
    }
}

/// 根据查询参数解析上传下载所用的本地用户。
fn resolve_query_user(query: &FilesQuery) -> Result<LocalUser, RpcError> {
    let username = query.username.as_deref().unwrap_or("root");
    resolve_user(username).map_err(auth_error)
}

/// 校验查询路径并在目标用户的主目录上下文中解析它。
fn resolve_query_path(query: &FilesQuery) -> Result<PathBuf, RpcError> {
    let raw_path = query
        .path
        .as_deref()
        .ok_or_else(|| RpcError::invalid_argument("path query parameter is required"))?;
    let user = resolve_query_user(query)?;
    resolve_path(raw_path, &user).map_err(|error| RpcError::invalid_argument(error.to_string()))
}

/// 将原始 HTTP 请求体写入同目录临时文件后原子替换目标文件。
async fn write_atomically(path: &Path, user: &LocalUser, body: Body) -> Result<(), RpcError> {
    let parent = path
        .parent()
        .ok_or_else(|| RpcError::invalid_argument("file path must have a parent directory"))?;
    ensure_parent_dirs(parent, user).await?;

    let mut temporary = create_temporary_file(parent).await?;
    if let Err(error) = write_temporary_file(&mut temporary, body).await {
        temporary.discard().await;
        return Err(error);
    }
    if let Err(error) = temporary.finish(path, user).await {
        temporary.discard().await;
        return Err(error);
    }

    Ok(())
}

/// 逐个处理 multipart 文件字段并返回所有成功上传的条目。
async fn upload_multipart(
    query: &FilesQuery,
    user: &LocalUser,
    content_type: &str,
    body: Body,
) -> Result<Response, RpcError> {
    let boundary = multer::parse_boundary(content_type).map_err(|error| {
        RpcError::invalid_argument(format!("invalid multipart request: {error}"))
    })?;
    let mut multipart = multer::Multipart::new(body.into_data_stream(), boundary);
    let mut uploaded = Vec::new();

    while let Some(mut field) = multipart.next_field().await.map_err(multipart_error)? {
        if field.name() != Some("file") {
            while field.chunk().await.map_err(multipart_error)?.is_some() {}
            continue;
        }

        let raw_path = match query.path.as_deref() {
            Some(path) => path,
            None => field.file_name().ok_or_else(|| {
                RpcError::invalid_argument("multipart file part requires a filename")
            })?,
        };
        let path = resolve_path(raw_path, user)
            .map_err(|error| RpcError::invalid_argument(error.to_string()))?;
        write_multipart_field_atomically(&path, user, &mut field).await?;
        uploaded.push(upload_entry(&path));
    }

    Ok(Json(uploaded).into_response())
}

/// 将一个 multipart 字段先写入临时文件，再原子替换目标文件。
async fn write_multipart_field_atomically(
    path: &Path,
    user: &LocalUser,
    field: &mut multer::Field<'_>,
) -> Result<(), RpcError> {
    let parent = path
        .parent()
        .ok_or_else(|| RpcError::invalid_argument("file path must have a parent directory"))?;
    ensure_parent_dirs(parent, user).await?;
    let mut temporary = create_temporary_file(parent).await?;
    if let Err(error) = write_multipart_field(&mut temporary, field).await {
        temporary.discard().await;
        return Err(error);
    }
    if let Err(error) = temporary.finish(path, user).await {
        temporary.discard().await;
        return Err(error);
    }

    Ok(())
}

/// 将 multipart 字段分块写入已预留的临时文件并同步到磁盘。
async fn write_multipart_field(
    temporary: &mut TemporaryUpload,
    field: &mut multer::Field<'_>,
) -> Result<(), RpcError> {
    while let Some(chunk) = field.chunk().await.map_err(multipart_error)? {
        temporary
            .write_all(&chunk)
            .await
            .map_err(|error| file_error(&temporary.path, error))?;
    }
    temporary.sync_all().await
}

/// 为上传响应构造文件条目元数据。
fn upload_entry(path: &Path) -> UploadEntry {
    UploadEntry {
        name: path
            .file_name()
            .and_then(|name| name.to_str())
            .unwrap_or_default()
            .into(),
        path: path.display().to_string(),
        r#type: "file",
    }
}

/// 创建缺失父目录，并将新目录的所有权设为请求用户。
async fn ensure_parent_dirs(parent: &Path, user: &LocalUser) -> Result<(), RpcError> {
    let mut missing = Vec::new();
    let mut current = parent;
    while fs::symlink_metadata(current).await.is_err() {
        missing.push(current.to_path_buf());
        current = current
            .parent()
            .ok_or_else(|| RpcError::invalid_argument("could not resolve parent directory"))?;
    }

    fs::create_dir_all(parent)
        .await
        .map_err(|error| file_error(parent, error))?;
    for directory in missing.into_iter().rev() {
        chown(&directory, user).await?;
    }

    Ok(())
}

/// 已预留的临时上传文件：从创建到 rename 全程持有 fd。
///
/// 写入、改属主（`fchown`）与改权限（`fchmod`）都作用在这个 fd 上，不再按路径重开
/// 或用路径版 `chown`——后者会被目标目录中有写权限的第三方用符号链接替换，从而让
/// 以 root 运行的 envd 把内容写进任意文件、或把任意 inode 改属主。
struct TemporaryUpload {
    /// 临时文件路径，仅用于最终 rename 与错误清理。
    path: PathBuf,
    /// 持有写入与属性修改所依赖的 fd。
    file: File,
}

impl TemporaryUpload {
    /// 分块写入请求体数据。
    async fn write_all(&mut self, chunk: &[u8]) -> std::io::Result<()> {
        self.file.write_all(chunk).await
    }

    /// 将数据刷入磁盘。
    async fn sync_all(&self) -> Result<(), RpcError> {
        self.file
            .sync_all()
            .await
            .map_err(|error| file_error(&self.path, error))
    }

    /// 收尾：冻结 fd 上的写入、改属主与权限，然后原子替换目标。
    ///
    /// 顺序与既有行为一致：先 chown（会清掉 setuid/setgid），再按目标原有权限位
    /// 设置 fchmod，最后 rename。
    async fn finish(&mut self, target: &Path, user: &LocalUser) -> Result<(), RpcError> {
        self.fchown(user).await?;
        if let Some(mode) = target_mode(target).await {
            nix::sys::stat::fchmod(&self.file, nix::sys::stat::Mode::from_bits_truncate(mode))
                .map_err(|error| {
                    RpcError::new(
                        Code::Internal,
                        format!("set permissions for {}: {error}", self.path.display()),
                    )
                })?;
        }
        fs::rename(&self.path, target)
            .await
            .map_err(|error| file_error(target, error))
    }

    /// 在线程池中按 fd 修改文件所有权，避免阻塞异步运行时。
    async fn fchown(&self, user: &LocalUser) -> Result<(), RpcError> {
        let file =
            self.file.try_clone().await.map_err(|error| {
                RpcError::new(Code::Internal, format!("clone upload fd: {error}"))
            })?;
        let uid = nix::unistd::Uid::from_raw(user.uid);
        let gid = nix::unistd::Gid::from_raw(user.gid);
        let path = self.path.clone();
        task::spawn_blocking(move || nix::unistd::fchown(&file, Some(uid), Some(gid)))
            .await
            .map_err(|error| {
                RpcError::new(Code::Internal, format!("join ownership task: {error}"))
            })?
            .map_err(|error| {
                RpcError::new(
                    Code::Internal,
                    format!("set ownership for {}: {error}", path.display()),
                )
            })
    }

    /// 删除临时文件（失败路径清理）。忽略清理本身的错误。
    async fn discard(self) {
        drop(self.file);
        let _ = fs::remove_file(&self.path).await;
    }
}

/// 在目标目录中预留唯一临时文件以保证后续 rename 原子性。
///
/// 文件名带每线程随机种子（`RandomState`）与进程内序号，避免可预测的名字被他人
/// 抢先占位或替换；`create_new` 保证不会复用已存在的路径。
async fn create_temporary_file(parent: &Path) -> Result<TemporaryUpload, RpcError> {
    for _ in 0..32 {
        let sequence = TEMP_SEQUENCE.fetch_add(1, Ordering::Relaxed);
        let entropy = std::collections::hash_map::RandomState::new()
            .build_hasher()
            .finish();
        let candidate = parent.join(format!(".cube-envd-upload-{entropy:016x}-{sequence}"));
        match OpenOptions::new()
            .write(true)
            .create_new(true)
            .open(&candidate)
            .await
        {
            Ok(file) => {
                return Ok(TemporaryUpload {
                    path: candidate,
                    file,
                })
            }
            Err(error) if error.kind() == std::io::ErrorKind::AlreadyExists => continue,
            Err(error) => return Err(file_error(&candidate, error)),
        }
    }

    Err(RpcError::new(
        Code::ResourceExhausted,
        "could not reserve a temporary upload file",
    ))
}

/// 将原始请求体流式写入已预留的临时文件并同步到磁盘。
async fn write_temporary_file(temporary: &mut TemporaryUpload, body: Body) -> Result<(), RpcError> {
    let mut stream = body.into_data_stream();
    while let Some(chunk) = stream.next().await {
        let chunk = chunk.map_err(|error| RpcError::new(Code::Internal, error.to_string()))?;
        temporary
            .write_all(&chunk)
            .await
            .map_err(|error| file_error(&temporary.path, error))?;
    }
    temporary.sync_all().await
}

/// 读取目标文件当前的权限位；目标不存在时返回 None。
///
/// 覆盖已有目标时保留其权限位：rename 会用临时文件的默认位替换目标，若不还原，
/// 可执行脚本或 0600 私钥会被重置为 0666 & umask（通常 0644）。
async fn target_mode(target: &Path) -> Option<u32> {
    let metadata = fs::metadata(target).await.ok()?;
    if !metadata.is_file() {
        return None;
    }
    Some(metadata.permissions().mode())
}

/// 在线程池中修改文件所有权，避免阻塞异步运行时。
async fn chown(path: &Path, user: &LocalUser) -> Result<(), RpcError> {
    let path = path.to_path_buf();
    let chown_path = path.clone();
    let uid = user.uid;
    let gid = user.gid;
    task::spawn_blocking(move || {
        nix::unistd::chown(
            &chown_path,
            Some(nix::unistd::Uid::from_raw(uid)),
            Some(nix::unistd::Gid::from_raw(gid)),
        )
    })
    .await
    .map_err(|error| RpcError::new(Code::Internal, format!("join ownership task: {error}")))?
    .map_err(|error| {
        RpcError::new(
            Code::Internal,
            format!("set ownership for {}: {error}", path.display()),
        )
    })
}

/// 将账户解析错误映射为认证失败。
fn auth_error(error: AuthError) -> RpcError {
    RpcError::new(Code::Unauthenticated, error.to_string())
}

/// 将 multipart 解析错误映射为无效请求。
fn multipart_error(error: multer::Error) -> RpcError {
    RpcError::invalid_argument(format!("invalid multipart request: {error}"))
}

/// 将底层文件系统错误映射为稳定的 RPC 错误码。
fn file_error(path: &Path, error: std::io::Error) -> RpcError {
    if error.kind() == std::io::ErrorKind::NotFound {
        RpcError::new(
            Code::NotFound,
            format!("path {} was not found", path.display()),
        )
    } else if error.raw_os_error() == Some(libc::ENOSPC) {
        RpcError::new(
            Code::ResourceExhausted,
            format!("not enough space for {}", path.display()),
        )
    } else {
        RpcError::new(
            Code::Internal,
            format!("access {}: {error}", path.display()),
        )
    }
}
