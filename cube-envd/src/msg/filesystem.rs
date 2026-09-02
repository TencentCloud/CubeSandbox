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

pub fn file_type_of(meta: &std::fs::Metadata) -> &'static str {
    // shared `getEntryType` (entry.go:72-82): regular -> FILE, dir ->
    // DIRECTORY, symlink -> SYMLINK, anything else -> UnknownFileType.
    //
    // The SYMLINK arm is only reachable from direct callers/tests:
    // `entry_info` branches on the link first and never classifies a
    // non-followed symlink here (the metadata it passes in is either the
    // followed target or a non-link).
    if meta.is_dir() {
        "FILE_TYPE_DIRECTORY"
    } else if meta.file_type().is_symlink() {
        "FILE_TYPE_SYMLINK"
    } else if meta.is_file() {
        "FILE_TYPE_FILE"
    } else {
        "FILE_TYPE_UNSPECIFIED"
    }
}

/// `os.FileMode.String()`-style permission string (upstream `Permissions` =
/// `fileMode.String()`, Go io/fs fs.go:212-232): type bits render as prefix
/// characters in bit order (`dalTLDpSugct?` — dir 'd', symlink 'L', block
/// device 'D', char device 'D'+'c', fifo 'p', socket 'S', setuid 'u', setgid
/// 'g', sticky 't'), then the 9 rwx bits. '-' when no type bit is set. Note
/// Go renders the special bits as *prefix characters*, NOT as ls(1)'s
/// s/S/t-replacing-the-x-slot form.
pub fn permissions_string(meta: &std::fs::Metadata) -> String {
    use std::os::unix::fs::{FileTypeExt, MetadataExt};
    let ft = meta.file_type();
    // MetadataExt::mode() returns the raw st_mode (type bits included,
    // e.g. 0o100644 for a regular file). The masks below read only what we
    // want: the suid/sgid/sticky bits and the low 9 permission bits.
    let mode = meta.mode();
    let mut s = String::with_capacity(10);
    if meta.is_dir() {
        s.push('d');
    } else if ft.is_symlink() {
        s.push('L');
    } else if ft.is_block_device() {
        s.push('D');
    } else if ft.is_char_device() {
        // Go: ModeDevice | ModeCharDevice, rendered in bit order.
        s.push('D');
        s.push('c');
    } else if ft.is_fifo() {
        s.push('p');
    } else if ft.is_socket() {
        s.push('S');
    }
    if mode & 0o4000 != 0 {
        s.push('u');
    }
    if mode & 0o2000 != 0 {
        s.push('g');
    }
    if mode & 0o1000 != 0 {
        s.push('t');
    }
    if s.is_empty() {
        s.push('-');
    }
    for shift in [6u32, 3, 0] {
        let bits = (mode >> shift) & 0o7;
        s.push(if bits & 0o4 != 0 { 'r' } else { '-' });
        s.push(if bits & 0o2 != 0 { 'w' } else { '-' });
        s.push(if bits & 0o1 != 0 { 'x' } else { '-' });
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
    // Upstream GetEntryInfo (shared entry.go:19-68): the argument metadata is
    // the *lstat* result. For a symlink the entry type and `mode` come from
    // the followed target (os.Stat); when the target cannot be stat'd the
    // type is UnknownFileType and mode stays 0. Everything else — name, size,
    // permissions, owner/group, modifiedTime — describes the link itself.
    let link = meta.file_type().is_symlink();
    let target = if link {
        std::fs::metadata(path).ok()
    } else {
        None
    };
    let (file_type, mode) = match &target {
        Some(t) => (file_type_of(t), t.mode() & 0o777),
        // Dangling link (or any failed target stat): UnknownFileType, mode 0.
        None if link => ("FILE_TYPE_UNSPECIFIED", 0),
        // Non-link: classify the lstat metadata itself; `mode` is the Go
        // FileMode.Perm() (m & 0777) of the file, not st_mode with suid etc.
        None => (file_type_of(meta), meta.mode() & 0o777),
    };
    let symlink_target = if link {
        // Upstream followSymlink = filepath.EvalSymlinks (entry.go:86-94): the
        // fully-resolved absolute path, or the input path itself on failure
        // (a dangling link reports its own path as the target).
        Some(
            std::fs::canonicalize(path)
                .map(|p| p.to_string_lossy().into_owned())
                .unwrap_or_else(|_| path.to_string()),
        )
    } else {
        None
    };
    EntryInfo {
        name,
        file_type,
        path: path.to_string(),
        size: {
            let s = meta.size();
            (s != 0).then(|| s.to_string())
        },
        mode,
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
    fn permissions_special_bits_match_go_filemode_string() {
        // Go io/fs fs.go:212-232 renders setuid/setgid/sticky as PREFIX
        // characters ('u'/'g'/'t') in bit order, NOT as ls(1)'s s/S/t
        // replacing the x slot (4755 renders "urwxr-xr-x", never rws).
        let dir = tempfile::tempdir().unwrap();
        let file = dir.path().join("suid");
        std::fs::write(&file, b"x").unwrap();
        for (mode, want) in [
            (0o4755u32, "urwxr-xr-x"),
            (0o4644, "urw-r--r--"),
            (0o2755, "grwxr-xr-x"),
            (0o1777, "trwxrwxrwx"),
            (0o1666, "trw-rw-rw-"),
            (0o0755, "-rwxr-xr-x"),
        ] {
            std::fs::set_permissions(&file, std::fs::Permissions::from_mode(mode)).unwrap();
            let meta = std::fs::metadata(&file).unwrap();
            assert_eq!(permissions_string(&meta), want, "mode {mode:o}");
        }
    }

    #[test]
    fn entry_info_symlink_shapes_follow_target() {
        // Upstream GetEntryInfo (entry.go:19-68): the entry type and `mode`
        // describe the FOLLOWED target; size/permissions/owner describe the
        // link itself (lstat). Dangling: UnknownFileType (type key omitted,
        // proto3 zero) and mode 0 (omitted).
        use std::os::unix::fs::{symlink, PermissionsExt};
        let dir = tempfile::tempdir().unwrap();
        let target = dir.path().join("t.txt");
        std::fs::write(&target, b"hello").unwrap();
        // Pin the target modes explicitly (a tempdir inherits the ambient
        // umask, which would make the 0o644/0o755 assertions environment-
        // dependent).
        std::fs::set_permissions(&target, std::fs::Permissions::from_mode(0o644)).unwrap();
        let sub = dir.path().join("sub");
        std::fs::create_dir(&sub).unwrap();
        std::fs::set_permissions(&sub, std::fs::Permissions::from_mode(0o755)).unwrap();

        let link = dir.path().join("to_file");
        symlink(&target, &link).unwrap();
        let meta = std::fs::symlink_metadata(&link).unwrap();
        let v = serde_json::to_value(entry_info(link.to_str().unwrap(), &meta)).unwrap();
        assert_eq!(v["type"], "FILE_TYPE_FILE", "link-to-file -> target type");
        // mode = target Perm (0644 & 0777); permissions = link itself ('L').
        assert_eq!(v["mode"], 0o644);
        assert_eq!(v["permissions"], "Lrwxrwxrwx");
        // size = link body (len of the stored target string).
        assert_eq!(v["size"], target.to_str().unwrap().len().to_string());
        // symlinkTarget = EvalSymlinks-style resolved absolute path.
        assert_eq!(v["symlinkTarget"], target.to_str().unwrap());

        let dlink = dir.path().join("to_dir");
        symlink(&sub, &dlink).unwrap();
        let meta = std::fs::symlink_metadata(&dlink).unwrap();
        let v = serde_json::to_value(entry_info(dlink.to_str().unwrap(), &meta)).unwrap();
        assert_eq!(
            v["type"], "FILE_TYPE_DIRECTORY",
            "link-to-dir -> target type"
        );
        assert_eq!(v["mode"], 0o755);
        assert_eq!(v["permissions"], "Lrwxrwxrwx");

        let dangling = dir.path().join("dangling");
        symlink(dir.path().join("gone"), &dangling).unwrap();
        let meta = std::fs::symlink_metadata(&dangling).unwrap();
        let v = serde_json::to_value(entry_info(dangling.to_str().unwrap(), &meta)).unwrap();
        assert!(
            v.get("type").is_none(),
            "dangling: UnknownFileType is the proto3 zero, key omitted: {v}"
        );
        assert!(
            v.get("mode").is_none(),
            "dangling: mode stays 0 (omitted): {v}"
        );
        assert_eq!(v["permissions"], "Lrwxrwxrwx");
        assert_eq!(
            v["symlinkTarget"],
            dangling.to_str().unwrap(),
            "EvalSymlinks fails -> the link's own path"
        );
    }

    #[test]
    fn entry_info_mode_is_go_perm() {
        // Mode field = Go FileMode.Perm() = m & 0777: setuid does NOT appear.
        let dir = tempfile::tempdir().unwrap();
        let file = dir.path().join("suid");
        std::fs::write(&file, b"x").unwrap();
        std::fs::set_permissions(&file, std::fs::Permissions::from_mode(0o4755)).unwrap();
        let meta = std::fs::metadata(&file).unwrap();
        let v = serde_json::to_value(entry_info(file.to_str().unwrap(), &meta)).unwrap();
        assert_eq!(v["mode"], 0o755, "Perm() strips the setuid bit");
        assert_eq!(v["permissions"], "urwxr-xr-x");
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
