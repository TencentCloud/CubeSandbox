// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Serde shapes for `spec/filesystem/filesystem.proto` (2026.16 baseline — no
//! xattr metadata / include_entry fields).
//!
//! This module is pure data: message structs, their field renames, and the
//! proto3 JSON omission rules. Anything that has to look at an actual file —
//! the metadata to `EntryInfo` mapping, the Go `FileMode` rendering, the
//! owner lookup — lives in `filesystem::entry`, so this file can be diffed
//! against the `.proto` without a syscall in sight.
use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, Deserialize)]
pub struct PathRequest {
    #[serde(default)]
    pub path: String,
}

#[derive(Debug, Clone, Deserialize)]
pub struct ListDirRequest {
    #[serde(default)]
    pub path: String,
    #[serde(default)]
    pub depth: u32,
}

#[derive(Debug, Clone, Deserialize)]
pub struct MoveRequest {
    #[serde(default)]
    pub source: String,
    #[serde(default)]
    pub destination: String,
}

#[derive(Debug, Clone, Serialize)]
pub struct EntryInfo {
    pub name: String,
    /// proto3 JSON omits the default enum value: `FILE_TYPE_UNSPECIFIED` (0)
    /// serializes with no `type` key at all (upstream dangling-symlink entry
    /// is `{name, path}` only — GetEntryInfo entry.go:53 sets UnknownFileType
    /// and the JSON proto encoder drops the zero).
    #[serde(rename = "type", skip_serializing_if = "is_unspecified")]
    pub file_type: &'static str,
    pub path: String,
    /// proto3 JSON renders int64 as a string and OMITS the default value: a
    /// zero-length file has no `size` key at all (matches Go envd). Stored as
    /// an Option so `0` disappears rather than serializing as `"0"`.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub size: Option<String>,
    /// proto3 JSON omits the default: mode `0` (e.g. a `chmod 000` file) has
    /// no `mode` key. Non-zero modes serialize as a decimal number.
    #[serde(skip_serializing_if = "is_zero_u32")]
    pub mode: u32,
    pub permissions: String,
    pub owner: String,
    pub group: String,
    #[serde(rename = "modifiedTime")]
    pub modified_time: String,
    #[serde(rename = "symlinkTarget", skip_serializing_if = "Option::is_none")]
    pub symlink_target: Option<String>,
}

fn is_zero_u32(v: &u32) -> bool {
    *v == 0
}

fn is_unspecified(t: &str) -> bool {
    t == "FILE_TYPE_UNSPECIFIED"
}

#[derive(Debug, Clone, Serialize)]
pub struct EntryResponse {
    pub entry: EntryInfo,
}

#[derive(Debug, Clone, Serialize)]
pub struct ListDirResponse {
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub entries: Vec<EntryInfo>,
}

// ---- Watch family (spec/filesystem/filesystem.proto:83-135) ----

#[derive(Debug, Clone, Deserialize)]
pub struct WatchDirRequest {
    #[serde(default)]
    pub path: String,
    /// Upstream encodes recursion by appending `/...` to the watched path
    /// (`utils/rfsnotify.go:6-12`); here it stays a bool and drives the
    /// per-directory watch walk instead.
    #[serde(default)]
    pub recursive: bool,
}

/// `WatchDirResponse` proto3 JSON: the oneof flattens, so each frame is a
/// single-key object — `{"start":{}}` / `{"filesystem":{...}}` /
/// `{"keepalive":{}}` (verified against the SDKs: `filesystem.ts:289` reads
/// `data.filesystem` at top level). Externally-tagged enum gives exactly
/// that shape. Note this differs from the process stream, whose proto wraps
/// the oneof in an explicit `event` field.
#[derive(Debug, Clone, Serialize)]
pub enum WatchDirResponse {
    #[serde(rename = "start")]
    Start(StartEvent),
    #[serde(rename = "filesystem")]
    Filesystem(FilesystemEvent),
    #[serde(rename = "keepalive")]
    KeepAlive(serde_json::Map<String, serde_json::Value>),
}

/// Empty proto message — serializes as `{}`.
#[derive(Debug, Clone, Serialize)]
pub struct StartEvent {}

/// proto3 JSON omits the default enum value, so `EVENT_TYPE_UNSPECIFIED`
/// never reaches the wire: every emitted event carries a concrete type.
///
/// ⚠️ The declaration order here is the proto numbering (CREATE=1, WRITE=2,
/// REMOVE=3, RENAME=4, CHMOD=5) and is NOT the emission order. Upstream
/// expands one kernel event in the fixed order Create → Rename → Chmod →
/// Write → Remove (`watch.go:105-123`); see `filesystem/watch/tree.rs`.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
pub enum EventType {
    #[serde(rename = "EVENT_TYPE_CREATE")]
    Create,
    #[serde(rename = "EVENT_TYPE_WRITE")]
    Write,
    #[serde(rename = "EVENT_TYPE_REMOVE")]
    Remove,
    #[serde(rename = "EVENT_TYPE_RENAME")]
    Rename,
    #[serde(rename = "EVENT_TYPE_CHMOD")]
    Chmod,
}

#[derive(Debug, Clone, Serialize)]
pub struct FilesystemEvent {
    pub name: String,
    #[serde(rename = "type")]
    pub event_type: EventType,
}

// ---- Pull watchers (proto:105-126) ----

#[derive(Debug, Clone, Deserialize)]
pub struct CreateWatcherRequest {
    #[serde(default)]
    pub path: String,
    #[serde(default)]
    pub recursive: bool,
}

#[derive(Debug, Clone, Serialize)]
pub struct CreateWatcherResponse {
    #[serde(rename = "watcherId")]
    pub watcher_id: String,
}

#[derive(Debug, Clone, Deserialize)]
pub struct GetWatcherEventsRequest {
    #[serde(default, rename = "watcherId")]
    pub watcher_id: String,
}

#[derive(Debug, Clone, Serialize)]
pub struct GetWatcherEventsResponse {
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub events: Vec<FilesystemEvent>,
}

#[derive(Debug, Clone, Deserialize)]
pub struct RemoveWatcherRequest {
    #[serde(default, rename = "watcherId")]
    pub watcher_id: String,
}

#[derive(Debug, Clone, Serialize)]
pub struct RemoveWatcherResponse {}
