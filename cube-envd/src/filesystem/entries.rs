// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

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
use notify::{
    event::{ModifyKind, RenameMode},
    EventKind,
};
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

/// 按请求用户的主目录规则解析路径。
///
/// 空路径与上游一致地解析为请求用户的主目录（`filepath.Join(home, "")`）。
pub(super) fn resolve(path: &str, user: &LocalUser) -> Result<PathBuf, RpcError> {
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
///
/// 符号链接按上游语义处理：`type` 与数值 `mode` 取自**解析后的目标**（悬空链接
/// 为未指定类型、mode 0），`symlinkTarget` 为规范化后的绝对路径；`permissions`
/// 与 `size` 始终取自 lstat。
pub(super) async fn entry_info(path: &Path) -> Result<EntryInfo, RpcError> {
    let metadata = fs::symlink_metadata(path)
        .await
        .map_err(|error| filesystem_error(path, error))?;
    let path_for_lookup = path.to_path_buf();
    let uid = metadata.uid();
    let gid = metadata.gid();
    let (symlink_target, target) = if metadata.file_type().is_symlink() {
        let resolved = resolve_symlink(path).await;
        let target = fs::metadata(&resolved).await.ok();
        (Some(resolved.display().to_string()), target)
    } else {
        (None, None)
    };
    let (owner, group) = task::spawn_blocking(move || ownership_names(uid, gid))
        .await
        .map_err(|error| {
            RpcError::new(Code::Internal, format!("join ownership lookup: {error}"))
        })?;

    entry_from_metadata(
        &path_for_lookup,
        &metadata,
        symlink_target,
        target.as_ref(),
        owner,
        group,
    )
    .map_err(|error| filesystem_error(&path_for_lookup, error))
}

/// 解析符号链接为规范化绝对路径；失败（例如悬空链接）时回退为原路径。
///
/// 对应上游 `filepath.EvalSymlinks`，后者在出错时同样返回传入的路径。
async fn resolve_symlink(path: &Path) -> PathBuf {
    fs::canonicalize(path)
        .await
        .unwrap_or_else(|_| path.to_path_buf())
}

/// 由 lstat 元数据（以及符号链接目标元数据）构造协议条目。
///
/// 字段来源与上游一致：`type`/`mode` 取符号链接目标，`permissions`/`size`/`owner`/
/// `group`/`modifiedTime` 取 lstat。
fn entry_from_metadata(
    path: &Path,
    metadata: &std::fs::Metadata,
    symlink_target: Option<String>,
    target: Option<&std::fs::Metadata>,
    owner: String,
    group: String,
) -> std::io::Result<EntryInfo> {
    let raw_mode = metadata.mode();
    let (file_type, mode) = match &symlink_target {
        None => (file_type_of(raw_mode), raw_mode & 0o777),
        Some(_) => match target {
            Some(target) => (file_type_of(target.mode()), target.mode() & 0o777),
            None => (proto::FileType::Unspecified as i32, 0),
        },
    };
    let modified_time = metadata
        .modified()?
        .duration_since(std::time::UNIX_EPOCH)
        .map_err(|error| std::io::Error::new(std::io::ErrorKind::InvalidData, error))?;

    Ok(EntryInfo {
        name: path
            .file_name()
            .and_then(|name| name.to_str())
            .unwrap_or_default()
            .into(),
        r#type: file_type,
        path: path.display().to_string(),
        size: metadata.len() as i64,
        mode,
        permissions: permission_string(raw_mode),
        owner,
        group,
        modified_time: pbjson_types::Timestamp {
            seconds: modified_time.as_secs() as i64,
            nanos: modified_time.subsec_nanos() as i32,
        },
        symlink_target,
    })
}

/// 将 `st_mode` 的文件类型位映射为协议条目类型。
fn file_type_of(mode: u32) -> i32 {
    match mode & libc::S_IFMT {
        libc::S_IFDIR => proto::FileType::Directory as i32,
        libc::S_IFLNK => proto::FileType::Symlink as i32,
        libc::S_IFREG => proto::FileType::File as i32,
        _ => proto::FileType::Unspecified as i32,
    }
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
///
/// 与 [`entry_info`] 共用同一套上游语义（见 [`entry_from_metadata`]）。
pub(super) fn entry_info_sync(path: &Path) -> std::io::Result<EntryInfo> {
    let metadata = std::fs::symlink_metadata(path)?;
    let (symlink_target, target) = if metadata.file_type().is_symlink() {
        let resolved = std::fs::canonicalize(path).unwrap_or_else(|_| path.to_path_buf());
        let target = std::fs::metadata(&resolved).ok();
        (Some(resolved.display().to_string()), target)
    } else {
        (None, None)
    };
    let (owner, group) = ownership_names(metadata.uid(), metadata.gid());

    entry_from_metadata(
        path,
        &metadata,
        symlink_target,
        target.as_ref(),
        owner,
        group,
    )
}

/// 将 notify 事件类型映射为 Filesystem 协议事件类型。
///
/// 映射对齐上游 envd 的 fsnotify 语义（`fsnotify` 把 `IN_MOVED_TO` 记为 Create、
/// `IN_ATTRIB` 记为 Chmod）：
/// - `Name(From)` → RENAME，`Name(To)` → CREATE；
/// - `Name(Both)` 是 notify 在两端匹配后额外合成的重复帧，必须丢弃，否则一次改名会
///   产生四条事件（上游只有 RENAME + CREATE 两条）；
/// - `Metadata(_)` → CHMOD：notify 的 inotify 后端对 `IN_ATTRIB` 只发
///   `Metadata(Any)`，此前的 `Permissions|Ownership` 分支因此永远不可达，chmod/chown/
///   touch 全被误报为 WRITE。
pub(super) fn watch_event_kind(kind: EventKind) -> Option<proto::EventType> {
    match kind {
        EventKind::Create(_) => Some(proto::EventType::Create),
        EventKind::Remove(_) => Some(proto::EventType::Remove),
        EventKind::Modify(ModifyKind::Name(RenameMode::From)) => Some(proto::EventType::Rename),
        EventKind::Modify(ModifyKind::Name(RenameMode::To)) => Some(proto::EventType::Create),
        EventKind::Modify(ModifyKind::Name(RenameMode::Both)) => None,
        EventKind::Modify(ModifyKind::Metadata(_)) => Some(proto::EventType::Chmod),
        EventKind::Modify(_) => Some(proto::EventType::Write),
        _ => None,
    }
}

/// 判断路径是否为文件系统根目录或挂载点。
///
/// 挂载点判定用"父目录的 `st_dev` 与自身不同"这一标准做法：跨设备意味着该目录是另一个
/// 文件系统的挂载根。递归删除这类路径会越过本次操作的语义边界（例如清空 guest 根
/// 文件系统的子树、或删进某个卷），因此 `Remove` 必须拒绝。
pub(super) async fn is_filesystem_root_or_mount_point(path: &Path) -> Result<bool, RpcError> {
    let Some(parent) = path.parent() else {
        // 形如 "/"：没有父目录即为根。
        return Ok(true);
    };

    let target = fs::metadata(path)
        .await
        .map_err(|error| filesystem_error(path, error))?;
    let parent = fs::metadata(parent)
        .await
        .map_err(|error| filesystem_error(parent, error))?;

    Ok(target.dev() != parent.dev())
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

/// 按上游 `os.FileMode.String()` 渲染权限字符串：先输出类型与特殊位前缀字符
/// （至少一个），再输出九位 `rwx`。
///
/// 前缀顺序与 Go 的 `"dalTLDpSugct?"` 一致；字符设备同时带 `D` 与 `c`。因此普通
/// 文件是 `-rw-r--r--`、目录是 `drwxr-xr-x`、符号链接是 `Lrwxrwxrwx`——描述的是
/// lstat 自身，而非符号链接的目标。
fn permission_string(mode: u32) -> String {
    let mut permissions = String::with_capacity(10);
    let file_type = mode & libc::S_IFMT;
    if file_type == libc::S_IFDIR {
        permissions.push('d');
    }
    if file_type == libc::S_IFLNK {
        permissions.push('L');
    }
    if file_type == libc::S_IFBLK || file_type == libc::S_IFCHR {
        permissions.push('D');
    }
    if file_type == libc::S_IFIFO {
        permissions.push('p');
    }
    if file_type == libc::S_IFSOCK {
        permissions.push('S');
    }
    if mode & libc::S_ISUID != 0 {
        permissions.push('u');
    }
    if mode & libc::S_ISGID != 0 {
        permissions.push('g');
    }
    if file_type == libc::S_IFCHR {
        permissions.push('c');
    }
    if mode & libc::S_ISVTX != 0 {
        permissions.push('t');
    }
    if permissions.is_empty() {
        permissions.push('-');
    }

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
