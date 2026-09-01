use std::{
    os::unix::fs::MetadataExt,
    path::{Path, PathBuf},
    sync::Mutex as StdMutex,
};

use axum::{
    body::{to_bytes, Bytes},
    extract::Request,
};
use nix::unistd::{Gid, Group, Uid, User};
use notify::{event::ModifyKind, EventKind};
use tokio::{fs, task};

use crate::{
    auth::{request_user, LocalUser},
    connect::{require_unary, Code, RpcError, MAX_UNARY_JSON_BYTES},
    generated::filesystem as proto,
    paths::resolve_path,
};

use super::error::filesystem_error;
use super::model::EntryInfo;

pub(super) fn record_watch_failure(failure: &StdMutex<Option<RpcError>>, error: RpcError) {
    if let Ok(mut failure) = failure.lock() {
        if failure.is_none() {
            *failure = Some(error);
        }
    }
}

/// 校验一元 Connect 请求、解析执行用户并读取受限大小的请求体。
pub(super) async fn unary_request(request: Request) -> Result<(LocalUser, Bytes), RpcError> {
    require_unary(request.headers())?;
    let user = request_user(request.headers())
        .map_err(|error| RpcError::new(Code::Unauthenticated, error.to_string()))?;
    let body = to_bytes(request.into_body(), MAX_UNARY_JSON_BYTES)
        .await
        .map_err(|_| RpcError::new(Code::ResourceExhausted, "unary JSON request exceeds 1 MiB"))?;

    Ok((user, body))
}

/// 拒绝空路径并按请求用户的主目录规则解析路径。
pub(super) fn resolve(path: &str, user: &LocalUser) -> Result<PathBuf, RpcError> {
    if path.is_empty() {
        return Err(RpcError::invalid_argument("path must not be empty"));
    }
    resolve_path(path, user).map_err(|error| RpcError::invalid_argument(error.to_string()))
}

/// 以显式栈遍历目录，收集不超过请求深度的子条目。
pub(super) async fn collect_entries(
    path: &Path,
    depth: u32,
    entries: &mut Vec<EntryInfo>,
) -> Result<(), RpcError> {
    let mut directories = vec![(path.to_path_buf(), depth)];
    while let Some((directory_path, remaining_depth)) = directories.pop() {
        let mut directory = fs::read_dir(&directory_path)
            .await
            .map_err(|error| filesystem_error(&directory_path, error))?;
        let mut children = Vec::new();
        while let Some(child) = directory
            .next_entry()
            .await
            .map_err(|error| filesystem_error(&directory_path, error))?
        {
            children.push(child.path());
        }
        children.sort();

        let mut child_directories = Vec::new();
        for child in children {
            let metadata = fs::symlink_metadata(&child)
                .await
                .map_err(|error| filesystem_error(&child, error))?;
            entries.push(entry_info(&child).await?);
            if remaining_depth > 1 && metadata.file_type().is_dir() {
                child_directories.push(child);
            }
        }
        directories.extend(
            child_directories
                .into_iter()
                .rev()
                .map(|child| (child, remaining_depth - 1)),
        );
    }

    Ok(())
}

/// 创建缺失目录并为每个新目录设置请求用户的所有权。
pub(super) async fn ensure_owned_dirs(path: &Path, user: &LocalUser) -> Result<(), RpcError> {
    let mut missing = Vec::new();
    let mut current = path;
    while fs::symlink_metadata(current).await.is_err() {
        missing.push(current.to_path_buf());
        current = current
            .parent()
            .ok_or_else(|| RpcError::invalid_argument("could not resolve parent directory"))?;
    }
    fs::create_dir_all(path)
        .await
        .map_err(|error| filesystem_error(path, error))?;
    for directory in missing.into_iter().rev() {
        let directory_for_chown = directory.clone();
        let uid = user.uid;
        let gid = user.gid;
        task::spawn_blocking(move || {
            nix::unistd::chown(
                &directory_for_chown,
                Some(Uid::from_raw(uid)),
                Some(Gid::from_raw(gid)),
            )
        })
        .await
        .map_err(|error| RpcError::new(Code::Internal, format!("join ownership task: {error}")))?
        .map_err(|error| {
            RpcError::new(
                Code::Internal,
                format!("set ownership for {}: {error}", directory.display()),
            )
        })?;
    }

    Ok(())
}

/// 异步读取条目元数据，并在线程池中解析属主和属组名称。
pub(super) async fn entry_info(path: &Path) -> Result<EntryInfo, RpcError> {
    let metadata = fs::symlink_metadata(path)
        .await
        .map_err(|error| filesystem_error(path, error))?;
    let mode = metadata.mode();
    let file_type = if metadata.file_type().is_symlink() {
        proto::FileType::Symlink as i32
    } else if metadata.is_dir() {
        proto::FileType::Directory as i32
    } else if metadata.is_file() {
        proto::FileType::File as i32
    } else {
        proto::FileType::Unspecified as i32
    };
    let path_for_lookup = path.to_path_buf();
    let uid = metadata.uid();
    let gid = metadata.gid();
    let symlink_target = if metadata.file_type().is_symlink() {
        Some(
            fs::read_link(path)
                .await
                .map_err(|error| filesystem_error(path, error))?
                .display()
                .to_string(),
        )
    } else {
        None
    };
    let (owner, group) = task::spawn_blocking(move || ownership_names(uid, gid))
        .await
        .map_err(|error| {
            RpcError::new(Code::Internal, format!("join ownership lookup: {error}"))
        })?;

    let modified_time = metadata
        .modified()
        .map_err(|error| {
            RpcError::new(
                Code::Internal,
                format!("read mtime for {}: {error}", path_for_lookup.display()),
            )
        })?
        .duration_since(std::time::UNIX_EPOCH)
        .map_err(|error| {
            RpcError::new(
                Code::Internal,
                format!("invalid mtime for {}: {error}", path_for_lookup.display()),
            )
        })?;
    let seconds = modified_time.as_secs() as i64;
    let nanos = modified_time.subsec_nanos();

    Ok(EntryInfo {
        name: path
            .file_name()
            .and_then(|name| name.to_str())
            .unwrap_or_default()
            .into(),
        r#type: file_type,
        path: path.display().to_string(),
        size: metadata.len() as i64,
        mode: mode & 0o7777,
        permissions: permission_string(mode),
        owner,
        group,
        modified_time: pbjson_types::Timestamp {
            seconds,
            nanos: nanos as i32,
        },
        symlink_target,
    })
}

/// 将 UID 和 GID 映射为名称，找不到时回退为数字字符串。
fn ownership_names(uid: u32, gid: u32) -> (String, String) {
    let owner = User::from_uid(Uid::from_raw(uid))
        .ok()
        .flatten()
        .map(|user| user.name)
        .unwrap_or_else(|| uid.to_string());
    let group = Group::from_gid(Gid::from_raw(gid))
        .ok()
        .flatten()
        .map(|group| group.name)
        .unwrap_or_else(|| gid.to_string());
    (owner, group)
}

/// 为 notify 回调同步读取条目元数据，避免在回调中进入异步运行时。
pub(super) fn entry_info_sync(path: &Path) -> std::io::Result<EntryInfo> {
    let metadata = std::fs::symlink_metadata(path)?;
    let mode = metadata.mode();
    let file_type = if metadata.file_type().is_symlink() {
        proto::FileType::Symlink as i32
    } else if metadata.is_dir() {
        proto::FileType::Directory as i32
    } else if metadata.is_file() {
        proto::FileType::File as i32
    } else {
        proto::FileType::Unspecified as i32
    };
    let symlink_target = if metadata.file_type().is_symlink() {
        Some(std::fs::read_link(path)?.display().to_string())
    } else {
        None
    };
    let modified_time = metadata
        .modified()?
        .duration_since(std::time::UNIX_EPOCH)
        .map_err(|error| std::io::Error::new(std::io::ErrorKind::InvalidData, error))?;
    let (owner, group) = ownership_names(metadata.uid(), metadata.gid());

    Ok(EntryInfo {
        name: path
            .file_name()
            .and_then(|name| name.to_str())
            .unwrap_or_default()
            .into(),
        r#type: file_type,
        path: path.display().to_string(),
        size: metadata.len() as i64,
        mode: mode & 0o7777,
        permissions: permission_string(mode),
        owner,
        group,
        modified_time: pbjson_types::Timestamp {
            seconds: modified_time.as_secs() as i64,
            nanos: modified_time.subsec_nanos() as i32,
        },
        symlink_target,
    })
}

/// 将 notify 事件类型映射为 Filesystem 协议事件类型。
pub(super) fn watch_event_kind(kind: EventKind) -> Option<proto::EventType> {
    match kind {
        EventKind::Create(_) => Some(proto::EventType::Create),
        EventKind::Remove(_) => Some(proto::EventType::Remove),
        EventKind::Modify(ModifyKind::Name(_)) => Some(proto::EventType::Rename),
        EventKind::Modify(ModifyKind::Metadata(
            notify::event::MetadataKind::Permissions | notify::event::MetadataKind::Ownership,
        )) => Some(proto::EventType::Chmod),
        EventKind::Modify(_) => Some(proto::EventType::Write),
        _ => None,
    }
}

/// 在线程池中依据文件系统 magic number 判断路径是否位于网络挂载上。
pub(super) async fn is_network_mount(path: &Path) -> Result<bool, RpcError> {
    let path = path.to_path_buf();
    task::spawn_blocking(move || {
        let magic = nix::sys::statfs::statfs(&path)?.filesystem_type().0 as u64;
        Ok::<_, nix::Error>(matches!(
            magic,
            0x0000_6969 | 0xff53_4d42 | 0x517b | 0xfe53_4d42 | 0x6573_5546
        ))
    })
    .await
    .map_err(|error| RpcError::new(Code::Internal, format!("join statfs task: {error}")))?
    .map_err(|error| RpcError::new(Code::Internal, format!("inspect filesystem type: {error}")))
}

/// 将 Unix 权限位渲染为九位 rwx 字符串。
fn permission_string(mode: u32) -> String {
    let mut permissions = String::with_capacity(9);
    for bit in [
        0o400, 0o200, 0o100, 0o040, 0o020, 0o010, 0o004, 0o002, 0o001,
    ] {
        permissions.push(match bit {
            0o400 | 0o040 | 0o004 if mode & bit != 0 => 'r',
            0o200 | 0o020 | 0o002 if mode & bit != 0 => 'w',
            0o100 | 0o010 | 0o001 if mode & bit != 0 => 'x',
            _ => '-',
        });
    }
    permissions
}
