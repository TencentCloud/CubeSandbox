use crate::{
    connect::encode_frame,
    generated::filesystem as proto,
    wire,
};

/// 表示文件系统条目的 JSON 元数据。
pub(super) struct EntryInfo {
    /// 条目的基名。
    pub(super) name: String,
    /// 协议定义的文件类型枚举值。
    pub(super) r#type: i32,
    /// 条目的完整路径。
    pub(super) path: String,
    /// 文件大小，单位为字节。
    pub(super) size: i64,
    /// Unix 权限位。
    pub(super) mode: u32,
    /// 人类可读的 rwx 权限字符串。
    pub(super) permissions: String,
    /// 条目属主名称或数字 ID。
    pub(super) owner: String,
    /// 条目属组名称或数字 ID。
    pub(super) group: String,
    /// Unix 纪元后的秒和纳秒。
    pub(super) modified_time: pbjson_types::Timestamp,
    /// 符号链接指向的路径。
    pub(super) symlink_target: Option<String>,
}

/// 将内部文件条目转换为生成的 protobuf 响应类型。
pub(super) fn proto_entry(entry: EntryInfo) -> proto::EntryInfo {
    proto::EntryInfo {
        name: entry.name,
        r#type: entry.r#type,
        path: entry.path,
        size: entry.size,
        mode: entry.mode,
        permissions: entry.permissions,
        owner: entry.owner,
        group: entry.group,
        modified_time: Some(entry.modified_time),
        symlink_target: entry.symlink_target,
        metadata: std::collections::HashMap::new(),
    }
}

/// 将生成的 WatchDir 响应编码为普通 Connect 数据帧。
pub(super) fn watch_frame(response: proto::WatchDirResponse) -> Vec<u8> {
    let payload = wire::encode_json(&response).expect("serialize WatchDir response");
    encode_frame(0, &payload).expect("bounded WatchDir frame")
}
