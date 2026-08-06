// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Serde mappings for `spec/filesystem/filesystem.proto` (2026.16 baseline —
//! no xattr metadata / include_entry fields).
//!
//! Baseline-verified EntryInfo JSON:
//! ```json
//! {"name":"sub","type":"FILE_TYPE_DIRECTORY","path":"/p/sub","size":"4096",
//!  "mode":493,"permissions":"drwxr-xr-x","owner":"user","group":"user",
//!  "modifiedTime":"2026-08-06T17:11:16.690489260Z"}
//! ```
//! Note `size` is a string (proto3 JSON int64) and `mode` is decimal.

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
    #[serde(rename = "type")]
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

#[derive(Debug, Clone, Serialize)]
pub struct EntryResponse {
    pub entry: EntryInfo,
}

#[derive(Debug, Clone, Serialize)]
pub struct ListDirResponse {
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub entries: Vec<EntryInfo>,
}

pub fn file_type_of(meta: &std::fs::Metadata) -> &'static str {
    if meta.is_dir() {
        "FILE_TYPE_DIRECTORY"
    } else if meta.file_type().is_symlink() {
        "FILE_TYPE_SYMLINK"
    } else {
        "FILE_TYPE_FILE"
    }
}

/// `ls -l`-style permission string, e.g. `drwxr-xr-x` / `-rw-r--r--`.
pub fn permissions_string(meta: &std::fs::Metadata) -> String {
    use std::os::unix::fs::MetadataExt;
    let mode = meta.mode();
    let kind = if meta.is_dir() {
        'd'
    } else if meta.file_type().is_symlink() {
        'l'
    } else {
        '-'
    };
    let mut s = String::with_capacity(10);
    s.push(kind);
    // setuid/setgid/sticky render like Go's os.FileMode / ls(1): the special
    // bit replaces the x slot ('s'/'t' when executable, 'S'/'T' when not).
    let specials = [mode & 0o4000 != 0, mode & 0o2000 != 0, mode & 0o1000 != 0];
    for (i, shift) in [6u32, 3, 0].into_iter().enumerate() {
        let bits = (mode >> shift) & 0o7;
        s.push(if bits & 0o4 != 0 { 'r' } else { '-' });
        s.push(if bits & 0o2 != 0 { 'w' } else { '-' });
        let x = bits & 0o1 != 0;
        s.push(match (specials[i], x, i) {
            (true, true, 2) => 't',
            (true, false, 2) => 'T',
            (true, true, _) => 's',
            (true, false, _) => 'S',
            (false, true, _) => 'x',
            (false, false, _) => '-',
        });
    }
    s
}

/// RFC3339 UTC following the protobuf Timestamp JSON rules used by upstream
/// envd (baseline-verified): 0, 3, 6 or 9 fractional digits depending on
/// precision — `16:23:33Z`, `17:11:16.690489260Z`.
pub fn rfc3339_nanos(t: std::time::SystemTime) -> String {
    let d = t.duration_since(std::time::UNIX_EPOCH).unwrap_or_default();
    let secs = d.as_secs() as i64;
    let nanos = d.subsec_nanos();

    // Civil-from-days algorithm (Howard Hinnant), no chrono dependency.
    let days = secs.div_euclid(86_400);
    let secs_of_day = secs.rem_euclid(86_400);
    let (hh, mm, ss) = (
        secs_of_day / 3600,
        (secs_of_day % 3600) / 60,
        secs_of_day % 60,
    );
    let z = days + 719_468;
    let era = z.div_euclid(146_097);
    let doe = z.rem_euclid(146_097);
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
    let y = yoe + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let day = doy - (153 * mp + 2) / 5 + 1;
    let month = if mp < 10 { mp + 3 } else { mp - 9 };
    let year = if month <= 2 { y + 1 } else { y };

    let frac = if nanos == 0 {
        String::new()
    } else if nanos % 1_000_000 == 0 {
        format!(".{:03}", nanos / 1_000_000)
    } else if nanos % 1_000 == 0 {
        format!(".{:06}", nanos / 1_000)
    } else {
        format!(".{nanos:09}")
    };
    format!("{year:04}-{month:02}-{day:02}T{hh:02}:{mm:02}:{ss:02}{frac}Z")
}

pub fn entry_info(path: &str, meta: &std::fs::Metadata) -> EntryInfo {
    use std::os::unix::fs::MetadataExt;
    let name = std::path::Path::new(path)
        .file_name()
        .map(|n| n.to_string_lossy().into_owned())
        .unwrap_or_else(|| path.to_string());
    let symlink_target = if meta.file_type().is_symlink() {
        std::fs::read_link(path)
            .ok()
            .map(|t| t.to_string_lossy().into_owned())
    } else {
        None
    };
    EntryInfo {
        name,
        file_type: file_type_of(meta),
        path: path.to_string(),
        size: {
            let s = meta.size();
            (s != 0).then(|| s.to_string())
        },
        mode: meta.mode() & 0o7777,
        permissions: permissions_string(meta),
        owner: owner_name(meta.uid()),
        group: group_name(meta.gid()),
        modified_time: rfc3339_nanos(meta.modified().unwrap_or(std::time::UNIX_EPOCH)),
        symlink_target,
    }
}

fn owner_name(uid: u32) -> String {
    name_from_table("/etc/passwd", uid).unwrap_or_else(|| uid.to_string())
}

fn group_name(gid: u32) -> String {
    name_from_table("/etc/group", gid).unwrap_or_else(|| gid.to_string())
}

fn name_from_table(path: &str, id: u32) -> Option<String> {
    let content = std::fs::read_to_string(path).ok()?;
    for line in content.lines() {
        let fields: Vec<&str> = line.split(':').collect();
        if fields.len() >= 3 && fields[2].parse::<u32>().ok() == Some(id) {
            return Some(fields[0].to_string());
        }
    }
    None
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::PermissionsExt;

    #[test]
    fn permissions_and_type() {
        let dir = tempfile::tempdir().unwrap();
        let meta = std::fs::metadata(dir.path()).unwrap();
        assert_eq!(file_type_of(&meta), "FILE_TYPE_DIRECTORY");
        assert!(permissions_string(&meta).starts_with('d'));

        let file = dir.path().join("f");
        std::fs::write(&file, b"x").unwrap();
        let meta = std::fs::metadata(&file).unwrap();
        assert_eq!(file_type_of(&meta), "FILE_TYPE_FILE");
        assert!(permissions_string(&meta).starts_with('-'));
    }

    #[test]
    fn permissions_special_bits_match_ls() {
        // setuid/setgid/sticky must render like Go's os.FileMode / ls(1),
        // not be dropped (4755 is -rwsr-xr-x, never -rwxr-xr-x).
        let dir = tempfile::tempdir().unwrap();
        let file = dir.path().join("suid");
        std::fs::write(&file, b"x").unwrap();
        for (mode, want) in [
            (0o4755u32, "-rwsr-xr-x"),
            (0o4644, "-rwSr--r--"),
            (0o2755, "-rwxr-sr-x"),
            (0o1777, "-rwxrwxrwt"),
            (0o1666, "-rw-rw-rwT"),
            (0o0755, "-rwxr-xr-x"),
        ] {
            std::fs::set_permissions(&file, std::fs::Permissions::from_mode(mode)).unwrap();
            let meta = std::fs::metadata(&file).unwrap();
            assert_eq!(permissions_string(&meta), want, "mode {mode:o}");
        }
    }

    #[test]
    fn entry_info_size_is_string() {
        let dir = tempfile::tempdir().unwrap();
        let file = dir.path().join("f.txt");
        std::fs::write(&file, b"hello").unwrap();
        let meta = std::fs::metadata(&file).unwrap();
        let info = entry_info(file.to_str().unwrap(), &meta);
        let v = serde_json::to_value(&info).unwrap();
        assert_eq!(v["size"], "5");
        assert_eq!(v["type"], "FILE_TYPE_FILE");
        assert_eq!(v["name"], "f.txt");
        assert!(v["modifiedTime"].as_str().unwrap().ends_with('Z'));
        assert!(v.get("symlinkTarget").is_none());
    }

    #[test]
    fn entry_info_omits_proto3_zero_values() {
        // proto3 JSON: a zero-length file has no `size` key, and a mode-000
        // file has no `mode` key — matching Go envd's default-value omission.
        let dir = tempfile::tempdir().unwrap();
        let file = dir.path().join("empty");
        std::fs::write(&file, b"").unwrap();
        std::fs::set_permissions(&file, std::fs::Permissions::from_mode(0o000)).unwrap();
        let meta = std::fs::metadata(&file).unwrap();
        let v = serde_json::to_value(entry_info(file.to_str().unwrap(), &meta)).unwrap();
        assert!(v.get("size").is_none(), "zero size must be omitted: {v}");
        assert!(v.get("mode").is_none(), "zero mode must be omitted: {v}");
    }

    #[test]
    fn rfc3339_format() {
        // 2026-08-06T17:11:16.690489260Z == epoch 1786036276.690489260
        let t = std::time::UNIX_EPOCH + std::time::Duration::new(1_786_036_276, 690_489_260);
        assert_eq!(rfc3339_nanos(t), "2026-08-06T17:11:16.690489260Z");
        // protobuf Timestamp JSON: zero nanos → no fractional digits
        // (baseline: "2022-01-06T16:23:33Z").
        let t = std::time::UNIX_EPOCH + std::time::Duration::new(1_641_486_213, 0);
        assert_eq!(rfc3339_nanos(t), "2022-01-06T16:23:33Z");
        // millisecond precision → 3 digits; microsecond → 6 digits.
        let t = std::time::UNIX_EPOCH + std::time::Duration::new(0, 500_000_000);
        assert_eq!(rfc3339_nanos(t), "1970-01-01T00:00:00.500Z");
        let t = std::time::UNIX_EPOCH + std::time::Duration::new(0, 500_001_000);
        assert_eq!(rfc3339_nanos(t), "1970-01-01T00:00:00.500001Z");
    }
}
