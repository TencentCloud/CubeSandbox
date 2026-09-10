// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use axum::{
    extract::Request,
    response::{IntoResponse, Response},
    Json,
};
use tokio::fs;

use crate::{
    connect::{Code, RpcError},
    generated::filesystem as proto,
    wire,
};

use super::{
    entries::{
        collect_entries, ensure_owned_dirs, entry_info, is_filesystem_root_or_mount_point, resolve,
        unary_request,
    },
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
///
/// 已存在目录返回 `AlreadyExists`(409)、已存在非目录返回 `invalid_argument`(400)，
/// 与上游 envd 一致。
pub async fn make_dir(request: Request) -> Result<Response, RpcError> {
    let (user, body) = unary_request(request).await?;
    let request: proto::MakeDirRequest = wire::decode_json(&body, "MakeDir request")?;
    let path = resolve(&request.path, &user)?;
    match fs::metadata(&path).await {
        Ok(metadata) if metadata.is_dir() => {
            return Err(RpcError::new(
                Code::AlreadyExists,
                format!("directory already exists: {}", path.display()),
            ));
        }
        Ok(_) => {
            return Err(RpcError::invalid_argument(format!(
                "path already exists but it is not a directory: {}",
                path.display()
            )));
        }
        // 不存在（以及 stat 因其他原因失败）时继续创建，由 mkdir 报告真实错误。
        Err(_) => {}
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
///
/// 与上游 `os.RemoveAll` 一致：路径不存在视为成功（幂等），因此重复删除不会报错。
/// 另外拒绝删除文件系统根目录与挂载点——递归删除会越过本次操作的语义边界。
pub async fn remove(request: Request) -> Result<Response, RpcError> {
    let (user, body) = unary_request(request).await?;
    let request: proto::RemoveRequest = wire::decode_json(&body, "Remove request")?;
    let path = resolve(&request.path, &user)?;
    let metadata = match fs::symlink_metadata(&path).await {
        Ok(metadata) => metadata,
        // 已经不在了：与 os.RemoveAll 一样返回成功，保证删除可重试。
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            return Ok(Json(proto::RemoveResponse {}).into_response())
        }
        Err(error) => return Err(filesystem_error(&path, error)),
    };
    if metadata.file_type().is_dir() {
        if is_filesystem_root_or_mount_point(&path).await? {
            return Err(RpcError::invalid_argument(format!(
                "refusing to remove filesystem root or mount point {}",
                path.display()
            )));
        }
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
