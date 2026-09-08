// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Request-user resolution.
//!
//! Users arrive as `Authorization: Basic base64("<user>:")` (all SDKs) or as
//! a `username` query parameter on `/files`. The daemon is statically linked
//! (musl, no NSS), so users and groups are resolved by parsing /etc/passwd
//! and /etc/group directly — the same effective behavior as upstream envd
//! built with CGO_ENABLED=0.
//!
//! Baseline-verified default: when neither source names a user, operations
//! run as root.

use base64::Engine;
use std::collections::HashMap;
use std::sync::{Arc, Mutex, OnceLock};

#[derive(Debug, Clone, PartialEq)]
pub struct User {
    pub name: String,
    pub uid: u32,
    pub gid: u32,
    pub home: String,
    pub groups: Vec<u32>,
}

pub const DEFAULT_USER: &str = "root";

/// Extract the username from a Basic auth header value ("Basic <b64>").
/// The scheme is matched case-insensitively per RFC 7617. Returns None when
/// the header is absent or not parseable as Basic auth.
pub fn user_from_basic_auth(header: Option<&str>) -> Option<String> {
    let header = header?;
    let (scheme, value) = header.split_once(' ')?;
    if !scheme.eq_ignore_ascii_case("Basic") {
        return None;
    }
    let decoded = base64::engine::general_purpose::STANDARD
        .decode(value.trim())
        .ok()?;
    let text = String::from_utf8(decoded).ok()?;
    let name = text.split(':').next().unwrap_or("").to_string();
    (!name.is_empty()).then_some(name)
}

pub fn lookup_user(name: &str) -> Result<User, String> {
    lookup_user_in(name, "/etc/passwd", "/etc/group")
}

struct CachedTable {
    dev: u64,
    ino: u64,
    mtime_secs: i64,
    mtime_nsecs: i64,
    content: Arc<String>,
}

impl CachedTable {
    fn stamp(&self) -> (u64, u64, i64, i64) {
        (self.dev, self.ino, self.mtime_secs, self.mtime_nsecs)
    }
}

/// Read a user/group table through a (dev, ino, mtime) cache.
///
/// Upstream re-reads these files per request (`permissions.GetUser`,
/// authenticate.go:22,38) and **per entry** in entryInfo (`utils.go:78-91`
/// does a LookupId/LookupGroupId each) — a deep ListDir re-reads the whole
/// file 2N times. The re-validation `stat` stays, so edits to the tables
/// are picked up on the next call; only the contents are cached.
///
/// Consistency: the stamp is taken before the read and re-taken after it
/// (file I/O happens outside the lock); the content is cached only if both
/// stamps agree, so the cached stamp always describes the cached bytes.
/// Residual limitation: an in-place edit preserving (dev, ino, mtime) is
/// invisible to any stamp-based scheme — accepted because passwd/group
/// editors (useradd/usermod) rewrite the file with a fresh mtime, and a
/// restore that also restores mtime would equally fool any stat-based
/// invalidation.
pub(crate) fn read_user_table(path: &str) -> Result<Arc<String>, std::io::Error> {
    static TABLES: OnceLock<Mutex<HashMap<String, CachedTable>>> = OnceLock::new();
    let tables = TABLES.get_or_init(|| Mutex::new(HashMap::new()));
    use std::os::unix::fs::MetadataExt;

    fn stamp_of(m: &std::fs::Metadata) -> (u64, u64, i64, i64) {
        (m.dev(), m.ino(), m.mtime(), m.mtime_nsec())
    }

    // Bounded: a table churning on every read (pathological) is served
    // uncached on the last attempt rather than spun on forever.
    for attempt in 0..4 {
        let pre = std::fs::metadata(path)?;
        let stamp = stamp_of(&pre);
        {
            let guard = tables.lock().unwrap_or_else(|p| p.into_inner());
            if let Some(cached) = guard.get(path) {
                if cached.stamp() == stamp {
                    return Ok(cached.content.clone());
                }
            }
        } // the file read below happens OUTSIDE the lock
        let bytes = std::fs::read_to_string(path)?;
        if stamp_of(&std::fs::metadata(path)?) != stamp {
            if attempt < 3 {
                continue; // edited mid-read; retry against the new state
            }
            return Ok(Arc::new(bytes));
        }
        let content = Arc::new(bytes);
        let mut guard = tables.lock().unwrap_or_else(|p| p.into_inner());
        // A concurrent fill under the same stamp holds equally-valid bytes;
        // keep it instead of churning the map.
        if let Some(cached) = guard.get(path) {
            if cached.stamp() == stamp {
                return Ok(cached.content.clone());
            }
        }
        guard.insert(
            path.to_string(),
            CachedTable {
                dev: stamp.0,
                ino: stamp.1,
                mtime_secs: stamp.2,
                mtime_nsecs: stamp.3,
                content: content.clone(),
            },
        );
        return Ok(content);
    }
    unreachable!("retry loop always returns within 4 attempts")
}

fn lookup_user_in(name: &str, passwd_path: &str, group_path: &str) -> Result<User, String> {
    let passwd = read_user_table(passwd_path)
        .map_err(|e| format!("error looking up user '{name}': reading {passwd_path}: {e}"))?;
    for line in passwd.lines() {
        let fields: Vec<&str> = line.split(':').collect();
        // name:passwd:uid:gid:gecos:home:shell
        if fields.len() >= 7 && fields[0] == name {
            let uid: u32 = fields[2]
                .parse()
                .map_err(|_| format!("error looking up user '{name}': bad uid"))?;
            let gid: u32 = fields[3]
                .parse()
                .map_err(|_| format!("error looking up user '{name}': bad gid"))?;
            return Ok(User {
                name: name.to_string(),
                uid,
                gid,
                home: fields[5].to_string(),
                groups: supplementary_groups(name, gid, group_path),
            });
        }
    }
    Err(format!(
        "error looking up user '{name}': user: unknown user {name}"
    ))
}

fn supplementary_groups(name: &str, primary_gid: u32, group_path: &str) -> Vec<u32> {
    let mut groups = vec![primary_gid];
    if let Ok(content) = read_user_table(group_path) {
        for line in content.lines() {
            let fields: Vec<&str> = line.split(':').collect();
            // group:passwd:gid:member1,member2
            if fields.len() >= 4 && fields[3].split(',').any(|m| m == name) {
                if let Ok(gid) = fields[2].parse::<u32>() {
                    if !groups.contains(&gid) {
                        groups.push(gid);
                    }
                }
            }
        }
    }
    groups
}

/// Resolve a request path the way upstream envd does: absolute paths are
/// used as-is, relative paths are anchored at the user's home directory.
pub fn resolve_path(path: &str, user: &User) -> String {
    if path.starts_with('/') {
        path.to_string()
    } else if let Some(rest) = path.strip_prefix("~/") {
        format!("{}/{}", user.home.trim_end_matches('/'), rest)
    } else if path == "~" {
        user.home.clone()
    } else {
        format!("{}/{}", user.home.trim_end_matches('/'), path)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{Seek, Write};

    fn fixture_files() -> (tempfile::NamedTempFile, tempfile::NamedTempFile) {
        let mut passwd = tempfile::NamedTempFile::new().unwrap();
        writeln!(passwd, "root:x:0:0:root:/root:/bin/bash").unwrap();
        writeln!(passwd, "user:x:1000:1000::/home/user:/bin/bash").unwrap();
        let mut group = tempfile::NamedTempFile::new().unwrap();
        writeln!(group, "user:x:1000:").unwrap();
        writeln!(group, "sudo:x:27:user,other").unwrap();
        writeln!(group, "docker:x:999:someoneelse").unwrap();
        (passwd, group)
    }

    #[test]
    fn basic_auth_parsing() {
        // base64("user:") == "dXNlcjo="
        assert_eq!(
            user_from_basic_auth(Some("Basic dXNlcjo=")),
            Some("user".to_string())
        );
        // RFC 7617: the scheme is case-insensitive.
        assert_eq!(
            user_from_basic_auth(Some("basic dXNlcjo=")),
            Some("user".to_string())
        );
        assert_eq!(
            user_from_basic_auth(Some("BASIC dXNlcjo=")),
            Some("user".to_string())
        );
        assert_eq!(user_from_basic_auth(Some("Bearer xyz")), None);
        assert_eq!(user_from_basic_auth(None), None);
    }

    #[test]
    fn passwd_lookup_and_groups() {
        let (passwd, group) = fixture_files();
        let u = lookup_user_in(
            "user",
            passwd.path().to_str().unwrap(),
            group.path().to_str().unwrap(),
        )
        .unwrap();
        assert_eq!(u.uid, 1000);
        assert_eq!(u.gid, 1000);
        assert_eq!(u.home, "/home/user");
        assert!(u.groups.contains(&1000));
        assert!(u.groups.contains(&27));
        assert!(!u.groups.contains(&999));

        let err = lookup_user_in(
            "ghost9",
            passwd.path().to_str().unwrap(),
            group.path().to_str().unwrap(),
        )
        .unwrap_err();
        // Baseline message: "error looking up user 'ghost9': user: unknown user ghost9"
        assert!(err.contains("unknown user ghost9"), "{err}");
    }

    #[test]
    fn path_resolution() {
        let u = User {
            name: "user".into(),
            uid: 1000,
            gid: 1000,
            home: "/home/user".into(),
            groups: vec![1000],
        };
        assert_eq!(resolve_path("/tmp/x", &u), "/tmp/x");
        assert_eq!(resolve_path("a.txt", &u), "/home/user/a.txt");
        assert_eq!(resolve_path("~/a.txt", &u), "/home/user/a.txt");
        assert_eq!(resolve_path("~", &u), "/home/user");
    }

    #[test]
    fn table_cache_invalidates_on_mtime_change() {
        let mut table = tempfile::NamedTempFile::new().unwrap();
        writeln!(table, "user:x:1000:1000::/home/user:/bin/bash").unwrap();
        let path = table.path().to_str().unwrap().to_string();

        let first = read_user_table(&path).unwrap();
        assert!(first.contains("1000"));
        // Unchanged file: the same Arc comes back from the cache.
        assert!(Arc::ptr_eq(&first, &read_user_table(&path).unwrap()));

        // In-place edit: rewrite content and bump mtime via set_times
        // (writing alone can land within the same timestamp granularity).
        table
            .as_file_mut()
            .seek(std::io::SeekFrom::Start(0))
            .unwrap();
        table
            .as_file_mut()
            .write_all(b"user:x:1001:1001::/home/user:/bin/bash\n")
            .unwrap();
        table
            .as_file_mut()
            .set_times(
                std::fs::FileTimes::new().set_modified(
                    std::time::SystemTime::now() + std::time::Duration::from_secs(10),
                ),
            )
            .unwrap();
        let second = read_user_table(&path).unwrap();
        assert!(second.contains("1001"), "stale cache served: {second}");
        assert!(!Arc::ptr_eq(&first, &second));
    }
}
