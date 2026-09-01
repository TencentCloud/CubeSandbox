use std::{
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

    let temporary = create_temporary_file(parent).await?;
    let outcome = write_temporary_file(&temporary, body).await;
    if let Err(error) = outcome {
        let _ = fs::remove_file(&temporary).await;
        return Err(error);
    }

    if let Err(error) = chown(&temporary, user).await {
        let _ = fs::remove_file(&temporary).await;
        return Err(error);
    }
    if let Err(error) = fs::rename(&temporary, path).await {
        let _ = fs::remove_file(&temporary).await;
        return Err(file_error(path, error));
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
    let temporary = create_temporary_file(parent).await?;
    let result = write_multipart_field(&temporary, field).await;
    if let Err(error) = result {
        let _ = fs::remove_file(&temporary).await;
        return Err(error);
    }
    if let Err(error) = chown(&temporary, user).await {
        let _ = fs::remove_file(&temporary).await;
        return Err(error);
    }
    if let Err(error) = fs::rename(&temporary, path).await {
        let _ = fs::remove_file(&temporary).await;
        return Err(file_error(path, error));
    }

    Ok(())
}

/// 将 multipart 字段分块写入预先创建的临时文件并同步到磁盘。
async fn write_multipart_field(path: &Path, field: &mut multer::Field<'_>) -> Result<(), RpcError> {
    let mut file = OpenOptions::new()
        .write(true)
        .truncate(true)
        .open(path)
        .await
        .map_err(|error| file_error(path, error))?;
    while let Some(chunk) = field.chunk().await.map_err(multipart_error)? {
        file.write_all(&chunk)
            .await
            .map_err(|error| file_error(path, error))?;
    }
    file.sync_all()
        .await
        .map_err(|error| file_error(path, error))?;

    Ok(())
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

/// 在目标目录中预留唯一临时文件以保证后续 rename 原子性。
async fn create_temporary_file(parent: &Path) -> Result<PathBuf, RpcError> {
    for _ in 0..32 {
        let sequence = TEMP_SEQUENCE.fetch_add(1, Ordering::Relaxed);
        let candidate = parent.join(format!(
            ".cube-envd-upload-{}-{sequence}",
            std::process::id()
        ));
        match OpenOptions::new()
            .write(true)
            .create_new(true)
            .open(&candidate)
            .await
        {
            Ok(file) => {
                drop(file);
                return Ok(candidate);
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
async fn write_temporary_file(path: &Path, body: Body) -> Result<(), RpcError> {
    let mut file = OpenOptions::new()
        .write(true)
        .truncate(true)
        .open(path)
        .await
        .map_err(|error| file_error(path, error))?;
    let mut stream = body.into_data_stream();
    while let Some(chunk) = stream.next().await {
        let chunk = chunk.map_err(|error| RpcError::new(Code::Internal, error.to_string()))?;
        file.write_all(&chunk)
            .await
            .map_err(|error| file_error(path, error))?;
    }
    file.sync_all()
        .await
        .map_err(|error| file_error(path, error))?;

    Ok(())
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
