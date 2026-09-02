// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Legacy SDK (User-Agent "connect-python") response downgrade.
//!
//! Mirrors upstream `packages/envd/internal/services/legacy/{interceptor,conversion}.go`
//! (Go envd 0.5.13). Scope note: upstream registers the interceptor ONLY on the
//! filesystem handler (`filesystem/service.go:28-31`), so this only affects
//! filesystem unary RPCs — in cube-envd that is exactly the `fs_unary!` macro in
//! `server.rs`. Process RPCs are untouched (the legacy process service is
//! defined in `legacyprocess.proto` but never mounted — dead code).

use axum::http::header::{HeaderMap, USER_AGENT};
use serde_json::Value;

pub const LEGACY_UA: &str = "connect-python";
pub const LEGACY_HEADER: &str = "X-E2B-Legacy-SDK";

/// Exact match on `User-Agent == "connect-python"` (upstream `interceptor.go:11-22`).
///
/// Upstream uses `http.Header.Get` which is case-insensitive on the key but an
/// exact `==` on the value, so `Connect-Python` / `connect-python/1.0` /
/// `connect-python ` (trailing space) do NOT trigger.
pub fn is_legacy(headers: &HeaderMap) -> bool {
    headers.get(USER_AGENT).and_then(|v| v.to_str().ok()) == Some(LEGACY_UA)
}

/// Downgrade a filesystem unary success response in place (upstream
/// `conversion.go:57-129`). Reached only via the `fs_unary!` macro, i.e. the
/// five filesystem unary RPCs cube-envd implements (Stat/ListDir/MakeDir/
/// Move/Remove — CreateWatcher and the rest of the watch family still answer
/// `unimplemented`, so they never reach here; when 1.1 mounts CreateWatcher its
/// `{watcherId}` has no entry/entries key and passes through untouched).
/// Only `entry` (Stat/Move/MakeDir) and `entries` (ListDir) are narrowed;
/// `Remove` already returns `{}`, which needs no code.
pub fn narrow(v: &mut Value) {
    let Some(obj) = v.as_object_mut() else { return };
    if let Some(e) = obj.get_mut("entry") {
        narrow_entry(e);
    }
    if let Some(entries) = obj.get_mut("entries") {
        if let Some(arr) = entries.as_array_mut() {
            for e in arr {
                narrow_entry(e);
            }
        }
    }
    // NOTE(1.1): WatchDir/GetWatcherEvents event narrowing is intentionally NOT
    // pre-coded here. The legacy event JSON shape (single `type` string vs the
    // current `eventTypes` list) is unresolved until 1.1 lands, so any placeholder
    // written now would be rewritten then (YAGNI). 1.1 adds the branch and reuses
    // `narrow_entry` / a `narrow_event` at that point. Header timing also differs
    // for streaming: upstream `interceptor.go:51-57` sets X-E2B-Legacy-SDK BEFORE
    // the handler runs, so streaming ERROR frames also carry the header — unlike
    // unary (see server.rs `fs_unary!`). 1.1 must align that timing, not just the
    // body narrowing.
}

/// Keep ONLY `{name, type, path}`; drop every other field (size/mode/permissions/
/// owner/group/modifiedTime/symlinkTarget) per upstream `conversion.go:16-26`.
///
/// Fixed 3-key WHITELIST — not "preserve all known fields"; the set is exactly
/// what upstream's legacy `EntryInfo` carries.
///
/// No value remapping happens here: the service layer classifies entries with
/// upstream `GetEntryInfo` semantics (links followed, dangling links
/// UNSPECIFIED), so the only types that can reach this whitelist are the three
/// legacy enum values — UNSPECIFIED (already omitted from the JSON by the
/// proto3-zero serialization), FILE, DIRECTORY.
fn narrow_entry(e: &mut Value) {
    if let Some(obj) = e.as_object_mut() {
        obj.retain(|k, _| matches!(k.as_str(), "name" | "type" | "path"));
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn is_legacy_exact_match() {
        let mut h = HeaderMap::new();
        h.insert(USER_AGENT, "connect-python".parse().unwrap());
        assert!(is_legacy(&h));

        // Missing header -> false.
        assert!(!is_legacy(&HeaderMap::new()));

        // Case / suffix / whitespace variants do NOT match (exact `==`).
        for ua in ["Connect-Python", "connect-python/1.0", "connect-python "] {
            let mut h = HeaderMap::new();
            h.insert(USER_AGENT, ua.parse().unwrap());
            assert!(!is_legacy(&h), "should not match: {ua:?}");
        }
    }

    #[test]
    fn narrow_entry_keeps_three_keys() {
        let mut v = serde_json::json!({
            "name": "sub",
            "type": "FILE_TYPE_DIRECTORY",
            "path": "/p/sub",
            "size": "4096",
            "mode": 493,
            "permissions": "drwxr-xr-x",
            "owner": "user",
            "group": "user",
            "modifiedTime": "2026-08-06T17:11:16.690489260Z",
            "symlinkTarget": null
        });
        // `narrow_entry` narrows the entry in place; `narrow` only unwraps the
        // `{entry:...}` / `{entries:...}` envelope. Test the leaf function directly.
        narrow_entry(&mut v);
        let obj = v.as_object().unwrap();
        assert_eq!(obj.len(), 3);
        assert!(obj.contains_key("name"));
        assert!(obj.contains_key("type"));
        assert!(obj.contains_key("path"));
        assert_eq!(obj.get("name").unwrap(), "sub");
        assert_eq!(obj.get("type").unwrap(), "FILE_TYPE_DIRECTORY");
        assert_eq!(obj.get("path").unwrap(), "/p/sub");
        assert!(obj.get("size").is_none());
        assert!(obj.get("permissions").is_none());
        assert!(obj.get("symlinkTarget").is_none());
    }

    #[test]
    fn narrow_entries_array_each_element() {
        let mut v = serde_json::json!({
            "entries": [
                {"name": "a", "type": "FILE_TYPE_FILE", "path": "/a", "size": "1", "mode": 420},
                {"name": "b", "type": "FILE_TYPE_DIRECTORY", "path": "/b", "size": "4096"}
            ]
        });
        narrow(&mut v);
        let arr = v["entries"].as_array().unwrap();
        assert_eq!(arr.len(), 2);
        // serde_json::Map is a BTreeMap, so retained keys are alphabetically
        // ordered (name, path, type) — order is irrelevant to JSON conformance,
        // only the key SET matters.
        for e in arr {
            let keys: std::collections::HashSet<&str> =
                e.as_object().unwrap().keys().map(|s| s.as_str()).collect();
            assert_eq!(
                keys,
                ["name", "path", "type"]
                    .into_iter()
                    .collect::<std::collections::HashSet<_>>()
            );
        }
    }

    #[test]
    fn narrow_entry_keeps_three_known_types_verbatim() {
        // The three in-enum values pass through untouched.
        for (t, path) in [
            ("FILE_TYPE_UNSPECIFIED", "/u"),
            ("FILE_TYPE_FILE", "/f"),
            ("FILE_TYPE_DIRECTORY", "/d"),
        ] {
            let mut v = serde_json::json!({"name": "n", "type": t, "path": path});
            narrow_entry(&mut v);
            assert_eq!(v["type"], serde_json::json!(t), "{t} must pass through");
        }
    }

    #[test]
    fn narrow_remove_is_empty_object() {
        // Remove already returns `{}`; narrowing must not change it.
        let mut v = serde_json::json!({});
        narrow(&mut v);
        assert!(v.as_object().unwrap().is_empty());
    }

    #[test]
    fn narrow_envelope_entry_narrows_inner() {
        // This is the real handler path: Stat/Move/MakeDir return {entry: <full>}.
        let mut v = serde_json::json!({
            "entry": {
                "name": "x", "type": "FILE_TYPE_FILE", "path": "/x",
                "size": "1", "mode": 420, "permissions": "-rw-r--r--",
                "owner": "user", "group": "user",
                "modifiedTime": "2026-08-06T17:11:16.690489260Z"
            }
        });
        narrow(&mut v);
        let inner = v["entry"].as_object().unwrap();
        assert_eq!(inner.len(), 3);
        assert!(inner.contains_key("name"));
        assert!(inner.contains_key("type"));
        assert!(inner.contains_key("path"));
    }

    #[test]
    fn narrow_passes_through_watcher_id() {
        // Shapes without entry/entries are untouched — e.g. CreateWatcher's
        // `{watcherId}` once 1.1 implements it (today it answers 501).
        let mut v = serde_json::json!({"watcherId": "abc123"});
        narrow(&mut v);
        assert_eq!(v, serde_json::json!({"watcherId": "abc123"}));
    }
}
