use axum::{
    extract::Request,
    response::{IntoResponse, Response},
    Json,
};
use tokio::fs;

use crate::{
    connect::RpcError,
    generated::filesystem as proto,
    wire,
};

use super::{
    entries::{collect_entries, ensure_owned_dirs, entry_info, resolve, unary_request},
    error::filesystem_error,
    model::proto_entry,
};

/// 返回指定路径的文件系统元数据。
pub async fn stat(request: Request) -> Result<Response, RpcError> {
    let (user, body) = unary_request(request).await?;
    let request: proto::StatRequest = wire::decode_json(&body, "Stat request")?;
    let path = resolve(&request.path, &user)?;
    Ok(Json(proto::StatResponse {
        entry: Some(proto_entry(entry_info(&path).await?)),
    })
    .into_response())
}

/// 创建目录及缺失父目录，并将它们归属给请求用户。
pub async fn make_dir(request: Request) -> Result<Response, RpcError> {
    let (user, body) = unary_request(request).await?;
    let request: proto::MakeDirRequest = wire::decode_json(&body, "MakeDir request")?;
    let path = resolve(&request.path, &user)?;
    if fs::symlink_metadata(&path).await.is_ok() {
        return Err(RpcError::invalid_argument(format!(
            "path {} already exists",
            path.display()
        )));
    }

    ensure_owned_dirs(&path, &user).await?;
    Ok(Json(proto::MakeDirResponse {
        entry: Some(proto_entry(entry_info(&path).await?)),
    })
    .into_response())
}

/// 将源条目移动到目标路径并返回移动后的元数据。
pub async fn move_entry(request: Request) -> Result<Response, RpcError> {
    let (user, body) = unary_request(request).await?;
    let request: proto::MoveRequest = wire::decode_json(&body, "Move request")?;
    let source = resolve(&request.source, &user)?;
    let destination = resolve(&request.destination, &user)?;
    let parent = destination
        .parent()
        .ok_or_else(|| RpcError::invalid_argument("destination must have a parent directory"))?;
    ensure_owned_dirs(parent, &user).await?;
    fs::rename(&source, &destination)
        .await
        .map_err(|error| filesystem_error(&source, error))?;

    Ok(Json(proto::MoveResponse {
        entry: Some(proto_entry(entry_info(&destination).await?)),
    })
    .into_response())
}

/// 按请求深度枚举目录条目并以稳定顺序返回。
pub async fn list_dir(request: Request) -> Result<Response, RpcError> {
    let (user, body) = unary_request(request).await?;
    let request: proto::ListDirRequest = wire::decode_json(&body, "ListDir request")?;
    let path = resolve(&request.path, &user)?;
    let metadata = fs::metadata(&path)
        .await
        .map_err(|error| filesystem_error(&path, error))?;
    if !metadata.is_dir() {
        return Err(RpcError::invalid_argument(format!(
            "path {} is not a directory",
            path.display()
        )));
    }

    let depth = request.depth.max(1);
    let mut entries = Vec::new();
    collect_entries(&path, depth, &mut entries).await?;
    entries.sort_by(|left, right| left.path.cmp(&right.path));
    let entries = entries.into_iter().map(proto_entry).collect();
    Ok(Json(proto::ListDirResponse { entries }).into_response())
}

/// 删除文件或递归删除目录。
pub async fn remove(request: Request) -> Result<Response, RpcError> {
    let (user, body) = unary_request(request).await?;
    let request: proto::RemoveRequest = wire::decode_json(&body, "Remove request")?;
    let path = resolve(&request.path, &user)?;
    let metadata = fs::symlink_metadata(&path)
        .await
        .map_err(|error| filesystem_error(&path, error))?;
    if metadata.file_type().is_dir() {
        fs::remove_dir_all(&path)
            .await
            .map_err(|error| filesystem_error(&path, error))?;
    } else {
        fs::remove_file(&path)
            .await
            .map_err(|error| filesystem_error(&path, error))?;
    }

    Ok(Json(proto::RemoveResponse {}).into_response())
}
