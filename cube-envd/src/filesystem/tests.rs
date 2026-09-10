// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use notify::event::{CreateKind, DataChange, MetadataKind, ModifyKind, RemoveKind, RenameMode};

use crate::generated::filesystem as proto;

use super::entries::watch_event_kind;

// 验证重命名两端的映射与上游一致：源端 RENAME、目标端 CREATE。
#[test]
fn rename_ends_map_to_rename_and_create() {
    assert_eq!(
        watch_event_kind(notify::EventKind::Modify(ModifyKind::Name(
            RenameMode::From
        ))),
        Some(proto::EventType::Rename)
    );
    assert_eq!(
        watch_event_kind(notify::EventKind::Modify(ModifyKind::Name(RenameMode::To))),
        Some(proto::EventType::Create)
    );
}

// 验证 notify 合成的 Name(Both) 重复帧被丢弃，一次改名只产生上游的两条事件。
#[test]
fn synthesized_rename_both_events_are_dropped() {
    assert_eq!(
        watch_event_kind(notify::EventKind::Modify(ModifyKind::Name(
            RenameMode::Both
        ))),
        None
    );
}

// 验证 chmod/chown/touch 映射为 CHMOD，而不是此前的 WRITE。
#[test]
fn metadata_changes_map_to_chmod() {
    for kind in [
        MetadataKind::Any,
        MetadataKind::Permissions,
        MetadataKind::Ownership,
        MetadataKind::WriteTime,
    ] {
        assert_eq!(
            watch_event_kind(notify::EventKind::Modify(ModifyKind::Metadata(kind))),
            Some(proto::EventType::Chmod),
            "metadata kind {kind:?} must map to CHMOD"
        );
    }
}

// 验证内容写入仍映射为 WRITE。
#[test]
fn data_changes_map_to_write() {
    assert_eq!(
        watch_event_kind(notify::EventKind::Modify(ModifyKind::Data(DataChange::Any))),
        Some(proto::EventType::Write)
    );
}

// 验证创建与删除事件类型保持不变。
#[test]
fn create_and_remove_kinds_are_preserved() {
    assert_eq!(
        watch_event_kind(notify::EventKind::Create(CreateKind::File)),
        Some(proto::EventType::Create)
    );
    assert_eq!(
        watch_event_kind(notify::EventKind::Remove(RemoveKind::File)),
        Some(proto::EventType::Remove)
    );
}

// 验证与协议无关的事件（如访问、内核队列溢出）不产生协议事件。
#[test]
fn unrelated_kinds_produce_no_event() {
    assert_eq!(
        watch_event_kind(notify::EventKind::Access(notify::event::AccessKind::Open(
            notify::event::AccessMode::Any
        ))),
        None
    );
    assert_eq!(watch_event_kind(notify::EventKind::Other), None);
    assert_eq!(watch_event_kind(notify::EventKind::Any), None);
}
