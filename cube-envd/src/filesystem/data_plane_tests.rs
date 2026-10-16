// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

use crate::platform::identity::User;
use std::os::unix::fs::PermissionsExt;

use super::errors::MAX_UPLOAD_SIZE;
use super::{download, entry_for, modtime_of, parse_boundary, spawn_upload_writer};

fn test_user(home: &str) -> User {
    User {
        name: "test".into(),
        uid: unsafe { libc::getuid() },
        gid: unsafe { libc::getgid() },
        home: home.into(),
        groups: vec![],
    }
}

#[tokio::test]
async fn upload_writer_creates_parents_and_writes_all_chunks() {
    let dir = tempfile::tempdir().unwrap();
    let user = test_user(dir.path().to_str().unwrap());
    let target = dir.path().join("a/b/c.txt");
    let (tx, rx) = tokio::sync::mpsc::channel(4);
    let writer = spawn_upload_writer(target.to_str().unwrap().to_string(), user.clone(), rx);
    // Chunk boundaries must not affect the content.
    for part in [&b"hel"[..], &b"lo"[..]] {
        tx.send(Ok(bytes::Bytes::copy_from_slice(part)))
            .await
            .unwrap();
    }
    drop(tx);
    writer.await.unwrap().unwrap();
    assert_eq!(std::fs::read(&target).unwrap(), b"hello");
    // In-place write: no `.cube-envd-upload` temp files ever exist.
    let leftovers: Vec<_> = std::fs::read_dir(target.parent().unwrap())
        .unwrap()
        .filter_map(|e| e.ok())
        .filter(|e| {
            e.file_name()
                .to_string_lossy()
                .starts_with(".cube-envd-upload")
        })
        .collect();
    assert!(leftovers.is_empty());
}

#[tokio::test]
async fn overwrite_keeps_mode_bits_automatically() {
    let dir = tempfile::tempdir().unwrap();
    let user = test_user(dir.path().to_str().unwrap());
    let target = dir.path().join("script.sh");
    // O_TRUNC never touches the mode of an existing file, so an
    // executable script keeps its x bits across an overwrite — matching
    // upstream, which carries no explicit mode copy.
    std::fs::write(&target, b"#!/bin/sh\n").unwrap();
    std::fs::set_permissions(&target, std::fs::Permissions::from_mode(0o755)).unwrap();
    let (tx, rx) = tokio::sync::mpsc::channel(4);
    let writer = spawn_upload_writer(target.to_str().unwrap().to_string(), user.clone(), rx);
    tx.send(Ok(bytes::Bytes::from_static(b"#!/bin/sh\necho v1\n")))
        .await
        .unwrap();
    drop(tx);
    writer.await.unwrap().unwrap();
    // O_TRUNC never touches the mode of an existing file.
    let (tx, rx) = tokio::sync::mpsc::channel(4);
    let writer = spawn_upload_writer(target.to_str().unwrap().to_string(), user.clone(), rx);
    tx.send(Ok(bytes::Bytes::from_static(b"#!/bin/sh\necho v2\n")))
        .await
        .unwrap();
    drop(tx);
    writer.await.unwrap().unwrap();
    let mode = std::fs::metadata(&target).unwrap().permissions().mode() & 0o7777;
    assert_eq!(mode, 0o755);
    // A fresh file still gets default create permissions.
    let fresh = dir.path().join("plain.txt");
    let (tx, rx) = tokio::sync::mpsc::channel(4);
    let writer = spawn_upload_writer(fresh.to_str().unwrap().to_string(), user.clone(), rx);
    tx.send(Ok(bytes::Bytes::from_static(b"x"))).await.unwrap();
    drop(tx);
    writer.await.unwrap().unwrap();
    assert!(std::fs::metadata(&fresh).unwrap().permissions().mode() & 0o111 == 0);
}

#[tokio::test]
async fn cap_exceeded_mid_stream_reports_413_and_keeps_partial_content() {
    let dir = tempfile::tempdir().unwrap();
    let user = test_user(dir.path().to_str().unwrap());
    let target = dir.path().join("cap.bin");
    // Two halves: the second chunk crosses the line and stops the write
    // before any of it lands — interrupted-write semantics (upstream has
    // no cap of its own; this is our documented 413 behavior).
    let half = MAX_UPLOAD_SIZE / 2 + 1;
    let (tx, rx) = tokio::sync::mpsc::channel(4);
    let writer = spawn_upload_writer(target.to_str().unwrap().to_string(), user, rx);
    tx.send(Ok(bytes::Bytes::from(vec![b'a'; half])))
        .await
        .unwrap();
    tx.send(Ok(bytes::Bytes::from(vec![b'b'; half])))
        .await
        .unwrap();
    drop(tx);
    let error = writer.await.unwrap().unwrap_err();
    assert_eq!(error.status, axum::http::StatusCode::PAYLOAD_TOO_LARGE);
    let content = std::fs::read(target).unwrap();
    assert_eq!(content.len(), half);
    assert!(content.iter().all(|&byte| byte == b'a'));
}

#[tokio::test]
async fn body_read_error_keeps_partial_content_and_propagates() {
    let dir = tempfile::tempdir().unwrap();
    let user = test_user(dir.path().to_str().unwrap());
    let target = dir.path().join("err.bin");
    let (tx, rx) = tokio::sync::mpsc::channel(4);
    let writer = spawn_upload_writer(target.to_str().unwrap().to_string(), user, rx);
    tx.send(Ok(bytes::Bytes::from_static(b"partial")))
        .await
        .unwrap();
    tx.send(Err(crate::protocol::RestError::new(
        axum::http::StatusCode::BAD_REQUEST,
        "error reading body: boom",
    )))
    .await
    .unwrap();
    drop(tx);
    let error = writer.await.unwrap().unwrap_err();
    assert_eq!(error.status, axum::http::StatusCode::BAD_REQUEST);
    assert_eq!(std::fs::read(target).unwrap(), b"partial");
}

#[tokio::test]
async fn upload_through_symlink_writes_target_and_leaves_link() {
    let dir = tempfile::tempdir().unwrap();
    let user = test_user(dir.path().to_str().unwrap());
    let real = dir.path().join("real.bin");
    let link = dir.path().join("lnk.bin");
    // In-place write follows a symlink (like upstream os.OpenFile), and
    // chown follows too (like upstream os.Chown): content lands on the
    // link's destination and the link itself survives. The ownership
    // differential (a daemon-owned target rather than the caller's uid)
    // needs a root-owned pre-existing target to observe — covered by the
    // container probe recorded in RESULTS.md.
    std::fs::write(&real, b"old").unwrap();
    std::os::unix::fs::symlink(&real, &link).unwrap();
    let (tx, rx) = tokio::sync::mpsc::channel(4);
    let writer = spawn_upload_writer(link.to_str().unwrap().to_string(), user, rx);
    tx.send(Ok(bytes::Bytes::from_static(b"new")))
        .await
        .unwrap();
    drop(tx);
    writer.await.unwrap().unwrap();
    assert_eq!(std::fs::read(real).unwrap(), b"new");
    assert!(std::fs::symlink_metadata(link)
        .unwrap()
        .file_type()
        .is_symlink());
}

#[tokio::test]
async fn multipart_read_error_awaits_writer_and_propagates() {
    let dir = tempfile::tempdir().unwrap();
    let target = dir.path().join("part.bin");
    let head = format!(
        "--X\r\nContent-Disposition: form-data; name=\"file\"; filename=\"{}\"\r\n\r\n",
        target.display()
    );
    let stream = futures::stream::unfold((0u8, Some(head)), |(step, head)| async move {
        tokio::time::sleep(std::time::Duration::from_millis(20)).await;
        match step {
            0 => Some((
                Ok::<bytes::Bytes, std::io::Error>(bytes::Bytes::from(head.unwrap())),
                (1, None),
            )),
            1 => Some((Ok(bytes::Bytes::from_static(b"more")), (2, None))),
            _ => Some((Err(std::io::Error::other("body died")), (3, None))),
        }
    });
    let mut headers = axum::http::HeaderMap::new();
    headers.insert(
        axum::http::header::CONTENT_TYPE,
        "multipart/form-data; boundary=X".parse().unwrap(),
    );
    let config = crate::platform::config::Config::new();
    let response = super::upload(
        &config,
        std::collections::HashMap::new(),
        headers,
        axum::body::Body::from_stream(stream),
    )
    .await;
    let (parts, body) = response.into_parts();
    let body = axum::body::to_bytes(body, 1 << 20).await.unwrap();
    assert_eq!(parts.status, axum::http::StatusCode::BAD_REQUEST);
    assert!(String::from_utf8_lossy(&body).contains("error reading multipart"));
    assert_eq!(std::fs::read(target).unwrap(), b"more");
}

#[test]
fn boundary_parsing_variants() {
    assert_eq!(
        parse_boundary("multipart/form-data; boundary=abc123").as_deref(),
        Some("abc123")
    );
    assert_eq!(
        parse_boundary("multipart/form-data; boundary=abc; charset=utf-8").as_deref(),
        Some("abc")
    );
    assert_eq!(
        parse_boundary(r#"multipart/form-data; boundary="quo;ted"; charset=utf-8"#).as_deref(),
        Some("quo;ted")
    );
    assert_eq!(parse_boundary("multipart/form-data"), None);
    assert_eq!(parse_boundary("multipart/form-data; boundary="), None);
}

#[test]
fn upload_entry_shape() {
    let e = entry_for("/home/user/hello.txt");
    let v = serde_json::to_value(&e).unwrap();
    assert_eq!(
        v,
        serde_json::json!({"name":"hello.txt","path":"/home/user/hello.txt","type":"file"})
    );
}
#[cfg(test)]
mod download_tests {
    use super::*;
    use crate::platform::config::Config;
    use axum::body::to_bytes;
    use axum::http::header;
    use axum::http::HeaderMap;
    use std::collections::HashMap;

    /// Drive the real GET /files handler against a tempdir file. The default
    /// user is root (present in /etc/passwd everywhere this suite runs); an
    /// absolute `path` query bypasses home anchoring. The config has no access
    /// token set, so the token gate passes.
    async fn get(path: &str, extra: &[(&str, &str)]) -> axum::response::Response {
        let config = Config::new();
        let mut params = HashMap::new();
        params.insert("path".to_string(), path.to_string());
        let mut headers = HeaderMap::new();
        for (k, v) in extra {
            headers.insert(k.parse::<header::HeaderName>().unwrap(), v.parse().unwrap());
        }
        download(&config, params, headers).await
    }

    async fn body(
        resp: axum::response::Response,
    ) -> (axum::http::StatusCode, axum::http::HeaderMap, Vec<u8>) {
        let (parts, b) = resp.into_parts();
        let bytes = to_bytes(b, 1 << 20).await.unwrap();
        (parts.status, parts.headers, bytes.to_vec())
    }

    /// 406 responses use the REST error shape (JSON code+message, like
    /// upstream jsonError) — the human message rides inside `message`.
    fn json_message(body: &[u8]) -> String {
        let v: serde_json::Value = serde_json::from_slice(body).unwrap();
        v["message"].as_str().unwrap().to_string()
    }

    fn tmp_file(name: &str, content: &[u8]) -> (tempfile::TempDir, String) {
        let dir = tempfile::tempdir().unwrap();
        let p = dir.path().join(name);
        std::fs::write(&p, content).unwrap();
        (dir, p.to_str().unwrap().to_string())
    }

    #[tokio::test]
    async fn plain_get_has_negotiation_headers() {
        let (_d, p) = tmp_file("base_a.txt", b"hello-octet\n");
        let resp = get(&p, &[]).await;
        let (status, h, b) = body(resp).await;
        assert_eq!(status, 200);
        assert_eq!(&b[..], b"hello-octet\n");
        assert_eq!(h[header::VARY], "Accept-Encoding");
        assert_eq!(h[header::ACCEPT_RANGES], "bytes");
        assert_eq!(h[header::CONTENT_TYPE], "text/plain; charset=utf-8");
        assert_eq!(h[header::CONTENT_LENGTH], "12");
        assert!(h.contains_key(header::LAST_MODIFIED)); // mtime is present
        assert_eq!(
            h[header::CONTENT_DISPOSITION],
            "inline; filename=base_a.txt"
        );
    }

    #[tokio::test]
    async fn range_returns_206_slice() {
        let (_d, p) = tmp_file("r.bin", b"0123456789");
        let resp = get(&p, &[("Range", "bytes=2-5")]).await;
        let (status, h, b) = body(resp).await;
        assert_eq!(status, 206);
        assert_eq!(&b[..], b"2345");
        assert_eq!(h[header::CONTENT_RANGE], "bytes 2-5/10");
        assert_eq!(h[header::CONTENT_LENGTH], "4");
        assert_eq!(h[header::ACCEPT_RANGES], "bytes");
    }

    #[tokio::test]
    async fn range_open_ended_and_suffix() {
        let (_d, p) = tmp_file("r.bin", b"0123456789");
        let resp = get(&p, &[("Range", "bytes=7-")]).await;
        let (status, h, b) = body(resp).await;
        assert_eq!(status, 206);
        assert_eq!(&b[..], b"789");
        assert_eq!(h[header::CONTENT_RANGE], "bytes 7-9/10");

        let resp = get(&p, &[("Range", "bytes=-3")]).await;
        let (_s, h, b) = body(resp).await;
        assert_eq!(&b[..], b"789");
        assert_eq!(h[header::CONTENT_RANGE], "bytes 7-9/10");
    }

    #[tokio::test]
    async fn if_modified_since_roundtrip_304() {
        let (_d, p) = tmp_file("c.txt", b"cache-me");
        let resp = get(&p, &[]).await;
        let (_s, h, _b) = body(resp).await;
        let lm = h[header::LAST_MODIFIED].to_str().unwrap().to_string();

        // Same Last-Modified → not modified.
        let resp = get(&p, &[("If-Modified-Since", &lm)]).await;
        let (status, h304, b) = body(resp).await;
        assert_eq!(status, 304);
        assert!(b.is_empty());
        assert_eq!(h304[header::LAST_MODIFIED], lm); // kept on 304
        assert_eq!(h304[header::VARY], "Accept-Encoding"); // preset survives
        assert!(!h304.contains_key(header::CONTENT_TYPE)); // none on 304
        assert!(!h304.contains_key(header::CONTENT_LENGTH));

        // Stale IMS (before the file's mtime) → full 200.
        let resp = get(
            &p,
            &[("If-Modified-Since", "Thu, 01 Jan 1970 00:00:00 GMT")],
        )
        .await;
        let (status, _h, b) = body(resp).await;
        assert_eq!(status, 200);
        assert_eq!(&b[..], b"cache-me");
    }

    #[tokio::test]
    async fn if_none_match_concrete_etag_serves_full_200() {
        // No ETag on the representation: a concrete If-None-Match never
        // matches → 200 full body, and If-Modified-Since is skipped (the
        // client's "changed" signal wins) — fs.go condTrue semantics.
        let (_d, p) = tmp_file("c.txt", b"etag-test");
        let resp = get(
            &p,
            &[
                ("If-None-Match", "\"deadbeef\""),
                ("If-Modified-Since", "Thu, 01 Jan 2099 00:00:00 GMT"),
            ],
        )
        .await;
        let (status, _h, b) = body(resp).await;
        assert_eq!(status, 200);
        assert_eq!(&b[..], b"etag-test");
    }

    #[tokio::test]
    async fn if_none_match_star_is_304() {
        let (_d, p) = tmp_file("c.txt", b"etag-test");
        let resp = get(&p, &[("If-None-Match", "*")]).await;
        let (status, _h, b) = body(resp).await;
        assert_eq!(status, 304);
        assert!(b.is_empty());
    }

    #[tokio::test]
    async fn empty_conditional_headers_are_absent_like_go() {
        // Go reads conditional headers with Header.Get, so a present-but-
        // empty header is the same as a missing one. Empty If-Match must not
        // 412 (a concrete etag would); empty If-None-Match must not
        // short-circuit If-Modified-Since.
        let (_d, p) = tmp_file("e.txt", b"0123456789");
        let (status, _h, _b) = body(get(&p, &[("If-Match", "")]).await).await;
        assert_eq!(status, 200);
        let lm = body(get(&p, &[]).await).await;
        let last_modified = lm.1[header::LAST_MODIFIED].to_str().unwrap().to_string();
        let resp = get(
            &p,
            &[("If-None-Match", ""), ("If-Modified-Since", &last_modified)],
        )
        .await;
        let (status, _h, b) = body(resp).await;
        assert_eq!(status, 304);
        assert!(b.is_empty());
    }

    #[tokio::test]
    async fn range_no_overlap_416_shape() {
        let (_d, p) = tmp_file("r.bin", b"0123456789");
        let resp = get(&p, &[("Range", "bytes=100-")]).await;
        let (status, h, b) = body(resp).await;
        assert_eq!(status, 416);
        assert_eq!(&b[..], b"invalid range: failed to overlap\n");
        assert_eq!(h[header::CONTENT_RANGE], "bytes */10");
        assert_eq!(h[header::CONTENT_TYPE], "text/plain; charset=utf-8");
        assert_eq!(h[header::X_CONTENT_TYPE_OPTIONS], "nosniff");
        // fs.go serveError strips Last-Modified from the 416 header set.
        assert!(!h.contains_key(header::LAST_MODIFIED));
        // Vary + Content-Disposition survive.
        assert_eq!(h[header::VARY], "Accept-Encoding");
        assert!(h.contains_key(header::CONTENT_DISPOSITION));
    }

    #[tokio::test]
    async fn range_bad_syntax_416_without_content_range() {
        let (_d, p) = tmp_file("r.bin", b"0123456789");
        let resp = get(&p, &[("Range", "bytes=abc")]).await;
        let (status, h, b) = body(resp).await;
        assert_eq!(status, 416);
        assert_eq!(&b[..], b"invalid range\n");
        assert!(!h.contains_key(header::CONTENT_RANGE));
    }

    #[tokio::test]
    async fn range_on_empty_file_ignored_200() {
        let (_d, p) = tmp_file("empty.bin", b"");
        // errNoOverlap + size == 0 → ignore Range, answer 200 (fs.go).
        let resp = get(&p, &[("Range", "bytes=0-")]).await;
        let (status, _h, b) = body(resp).await;
        assert_eq!(status, 200);
        assert!(b.is_empty());
    }

    /// A true empty file must send `Content-Length: 0` exactly like Go's
    /// Seek(End)-sized ServeContent — not chunked (the conformance normalizer
    /// hides framing, so it cannot catch this).
    #[tokio::test]
    async fn empty_file_sends_content_length_zero() {
        let (_d, p) = tmp_file("empty.bin", b"");
        let resp = get(&p, &[]).await;
        let (status, h, b) = body(resp).await;
        assert_eq!(status, 200);
        assert!(b.is_empty());
        assert_eq!(h[header::CONTENT_LENGTH], "0");
    }

    /// The other half of the Seek(End) model: /proc files seek fine at the
    /// start but report EINVAL on Seek(End) — Go's sizeFunc fails and
    /// ServeContent answers 500 "seeker can't seek" (fs.go errSeeker path).
    #[tokio::test]
    async fn proc_pseudo_file_answers_500_seeker_cant_seek() {
        // The 500 relies on the kernel answering EINVAL to SEEK_END on
        // procfs. Probe the actual seek behavior rather than trusting the
        // file type: if a runtime ever allowed it, both sides would answer
        // CL: 0 and this test must not assert cube-envd's mapping.
        match tokio::fs::File::open("/proc/self/status").await {
            Ok(mut f) => {
                use tokio::io::AsyncSeekExt;
                if f.seek(std::io::SeekFrom::End(0)).await.is_ok() {
                    return; // kernel allows SEEK_END here: skip
                }
            }
            Err(_) => return, // no procfs: nothing to assert
        }
        let resp = get("/proc/self/status", &[]).await;
        let (status, h, b) = body(resp).await;
        assert_eq!(status, 500);
        assert_eq!(&b[..], b"seeker can't seek\n");
        // fs.go serveError shape: text/plain + nosniff, Last-Modified
        // stripped, Vary/Content-Disposition kept.
        assert_eq!(h[header::CONTENT_TYPE], "text/plain; charset=utf-8");
        assert!(h.contains_key(header::X_CONTENT_TYPE_OPTIONS));
        assert!(!h.contains_key(header::LAST_MODIFIED));
        assert_eq!(h[header::VARY], "Accept-Encoding");
        assert!(h.contains_key(header::CONTENT_DISPOSITION));
    }

    /// An obs-text byte after a fractional second must not panic the date
    /// parser (byte index inside U+FFFD); the unparseable date is treated as
    /// "condition does not apply", not a 500.
    #[tokio::test]
    async fn obs_text_after_fractional_second_does_not_500() {
        let (_d, p) = tmp_file("c.bin", b"cache-me");
        let mut h = HeaderMap::new();
        h.insert(
            header::IF_MODIFIED_SINCE,
            header::HeaderValue::from_bytes(b"Sun, 06 Sep 2026 07:00:00.5\xFF1 GMT").unwrap(),
        );
        let (status, _h, b) = body(get_headers(&p, h).await).await;
        assert_eq!(status, 200);
        assert_eq!(&b[..], b"cache-me");
    }

    /// An asctime If-Modified-Since with a fractional second — the digit run
    /// must not swallow the year (Go answers 304).
    #[tokio::test]
    async fn asctime_fractional_second_ims_304s() {
        let t = std::time::UNIX_EPOCH
            .checked_add(std::time::Duration::from_secs(1_788_678_000))
            .unwrap();
        let (_d, p, kept) = tmp_file_with_mtime("f.txt", b"cache-me", t);
        if !kept {
            return; // filesystem clamped the stamp: nothing to assert
        }
        let resp = get(&p, &[("If-Modified-Since", "Sun Sep  6 07:00:00.5 2026")]).await;
        let (status, _h, _b) = body(resp).await;
        assert_eq!(status, 304);
    }

    /// Char devices with stat size 0 (Go Seek(End) = 0): the answer is an
    /// explicit CL: 0 with an empty body — never an unbounded chunked stream
    /// (verified against Go's ServeContent).
    #[tokio::test]
    async fn char_device_answers_empty_cl0_like_go() {
        if tokio::fs::metadata("/dev/zero")
            .await
            .map(|m| m.is_file())
            .unwrap_or(true)
        {
            return; // no /dev/zero here: nothing to assert
        }
        let resp = get("/dev/zero", &[]).await;
        let (status, h, b) = body(resp).await;
        assert_eq!(status, 200);
        assert!(b.is_empty());
        assert_eq!(h[header::CONTENT_LENGTH], "0");
    }

    #[tokio::test]
    async fn if_match_concrete_fails_412() {
        let (_d, p) = tmp_file("c.txt", b"412-body");
        let resp = get(&p, &[("If-Match", "\"etag\"")]).await;
        let (status, h, b) = body(resp).await;
        assert_eq!(status, 412);
        assert!(b.is_empty());
        assert!(!h.contains_key(header::CONTENT_TYPE));
        assert_eq!(h[header::VARY], "Accept-Encoding");
        // 412 keeps Last-Modified (bare WriteHeader, no serveError).
        assert!(h.contains_key(header::LAST_MODIFIED));
    }

    #[tokio::test]
    async fn range_with_identity_rejected_is_406_with_vary() {
        let (_d, p) = tmp_file("r.bin", b"0123456789");
        // `identity;q=0, gzip` parses fine (gzip acceptable) but rejects
        // identity → the Range/conditional gate's 406b, which carries Vary
        // (it answers after Vary was set).
        let resp = get(
            &p,
            &[
                ("Range", "bytes=0-1"),
                ("Accept-Encoding", "identity;q=0, gzip"),
            ],
        )
        .await;
        let (status, h, b) = body(resp).await;
        assert_eq!(status, 406);
        assert_eq!(
            json_message(&b),
            "identity encoding not acceptable for Range or conditional request"
        );
        assert_eq!(h[header::VARY], "Accept-Encoding");
    }

    #[tokio::test]
    async fn multi_range_served_as_full_200() {
        // No multipart writer: a multi-range request gets the whole file.
        let (_d, p) = tmp_file("r.bin", b"0123456789");
        let resp = get(&p, &[("Range", "bytes=0-1, 5-6")]).await;
        let (status, _h, b) = body(resp).await;
        assert_eq!(status, 200);
        assert_eq!(&b[..], b"0123456789");
    }

    #[tokio::test]
    async fn if_range_mismatched_etag_drops_range() {
        let (_d, p) = tmp_file("r.bin", b"0123456789");
        // If-Range with an etag never matches (no current etag) → Range is
        // dropped and the whole file is served.
        let resp = get(&p, &[("Range", "bytes=0-1"), ("If-Range", "\"old-etag\"")]).await;
        let (status, _h, b) = body(resp).await;
        assert_eq!(status, 200);
        assert_eq!(&b[..], b"0123456789");
    }

    #[tokio::test]
    async fn gzip_rejected_406_before_vary() {
        let (_d, p) = tmp_file("g.txt", b"x");
        // *;q=0 without an identity entry rejects everything → 406 answers
        // BEFORE Vary is set (upstream order).
        let resp = get(&p, &[("Accept-Encoding", "*;q=0")]).await;
        let (status, h, b) = body(resp).await;
        assert_eq!(status, 406);
        assert_eq!(
            json_message(&b),
            "error parsing Accept-Encoding: no acceptable encoding found, supported: [gzip]"
        );
        assert!(!h.contains_key(header::VARY));
    }

    // ---- mtime edge cases --------------------------------------------------

    use std::time::{Duration, SystemTime, UNIX_EPOCH};

    /// Create a file and stamp it with an exact mtime (std File::set_modified
    /// → futimens, nanosecond precision on Linux). Prefer /dev/shm: some
    /// filesystems (e.g. ext4) silently clamp far-future stamps. Returns
    /// whether the filesystem actually kept the stamp.
    fn tmp_file_with_mtime(
        name: &str,
        content: &[u8],
        mtime: SystemTime,
    ) -> (tempfile::TempDir, String, bool) {
        let dir = match tempfile::tempdir_in("/dev/shm") {
            Ok(d) => d,
            Err(_) => tempfile::tempdir().unwrap(),
        };
        let p = dir.path().join(name);
        std::fs::write(&p, content).unwrap();
        std::fs::File::open(&p)
            .unwrap()
            .set_modified(mtime)
            .unwrap();
        let got = dir
            .path()
            .join(name)
            .metadata()
            .unwrap()
            .modified()
            .unwrap();
        let kept = got == mtime;
        (dir, p.to_str().unwrap().to_string(), kept)
    }

    #[test]
    fn modtime_of_matches_go_is_zero_time_and_floor() {
        // Exactly the epoch → isZeroTime: no date conditions, but If-Range
        // still knows the floor second.
        assert_eq!(modtime_of(Ok(UNIX_EPOCH)), (None, Some(0)));
        // Sub-second after the epoch: NOT zero — Last-Modified prints the
        // floor (epoch date) and the date conditions compare the floor.
        let t = UNIX_EPOCH.checked_add(Duration::from_millis(500)).unwrap();
        assert_eq!(modtime_of(Ok(t)), (Some(0), Some(0)));
        // Pre-epoch mtimes are kept (not dropped); Go Truncate floors:
        // -1.5s → -2s.
        let t = UNIX_EPOCH.checked_sub(Duration::from_secs(1)).unwrap();
        assert_eq!(modtime_of(Ok(t)), (Some(-1), Some(-1)));
        let t = UNIX_EPOCH.checked_sub(Duration::from_millis(500)).unwrap();
        assert_eq!(modtime_of(Ok(t)), (Some(-1), Some(-1)));
        let t = UNIX_EPOCH.checked_sub(Duration::from_millis(1500)).unwrap();
        assert_eq!(modtime_of(Ok(t)), (Some(-2), Some(-2)));
    }

    /// `touch -d '@253402300800'` (year 10000, beyond the time crate's
    /// range): Go answers 200 with the file, so this must not panic into a
    /// 500.
    #[tokio::test]
    async fn out_of_range_mtime_downloads_without_panic() {
        let t = UNIX_EPOCH
            .checked_add(Duration::from_secs(253_402_300_800))
            .unwrap();
        let (_d, p, kept) = tmp_file_with_mtime("far.txt", b"0123456789abcdefghij", t);
        let resp = get(&p, &[]).await;
        let (status, h, b) = body(resp).await;
        assert_eq!(status, 200);
        assert_eq!(&b[..], b"0123456789abcdefghij");
        if kept {
            assert_eq!(h[header::LAST_MODIFIED], "Sat, 01 Jan 10000 00:00:00 GMT");
        }
        // (A filesystem that clamps the stamp keeps the test to the
        // no-panic/200 contract; tmpfs keeps it verbatim.)
    }

    /// mtime = epoch + 0.5s: isZeroTime does NOT fire (the raw time is not
    /// the epoch), so Last-Modified is present and a matching If-Modified-
    /// Since (the epoch date, which is what the header truncates to) 304s.
    #[tokio::test]
    async fn subsecond_epoch_mtime_keeps_last_modified_and_304s() {
        let t = UNIX_EPOCH.checked_add(Duration::from_millis(500)).unwrap();
        let (_d, p, _kept) = tmp_file_with_mtime("half.txt", b"cache-me", t);
        let resp = get(&p, &[]).await;
        let (_s, h, _b) = body(resp).await;
        assert_eq!(h[header::LAST_MODIFIED], "Thu, 01 Jan 1970 00:00:00 GMT");

        let resp = get(
            &p,
            &[("If-Modified-Since", "Thu, 01 Jan 1970 00:00:00 GMT")],
        )
        .await;
        let (status, _h, _b) = body(resp).await;
        assert_eq!(status, 304);
    }

    /// Pre-epoch mtime: Last-Modified is kept (floored) and round-trips.
    #[tokio::test]
    async fn pre_epoch_mtime_kept_and_304s() {
        let t = UNIX_EPOCH.checked_sub(Duration::from_millis(1500)).unwrap();
        let (_d, p, _kept) = tmp_file_with_mtime("old.txt", b"cache-me", t);
        let resp = get(&p, &[]).await;
        let (_s, h, _b) = body(resp).await;
        // -1.5s floors to -2s (Go Truncate).
        assert_eq!(h[header::LAST_MODIFIED], "Wed, 31 Dec 1969 23:59:58 GMT");

        let resp = get(
            &p,
            &[("If-Modified-Since", "Wed, 31 Dec 1969 23:59:58 GMT")],
        )
        .await;
        let (status, _h, _b) = body(resp).await;
        assert_eq!(status, 304);

        // An earlier IMS (before the mtime) → full 200.
        let resp = get(
            &p,
            &[("If-Modified-Since", "Wed, 31 Dec 1969 23:59:57 GMT")],
        )
        .await;
        let (status, _h, _b) = body(resp).await;
        assert_eq!(status, 200);
    }

    /// Exactly-epoch mtime: no Last-Modified, date conditions condNone — but
    /// If-Range has no zero-time gate in fs.go, so a matching epoch date
    /// keeps the Range → 206.
    #[tokio::test]
    async fn exact_epoch_mtime_hides_last_modified_but_keeps_if_range() {
        let (_d, p, _kept) = tmp_file_with_mtime("epoch.bin", b"0123456789", UNIX_EPOCH);
        let resp = get(&p, &[]).await;
        let (_s, h, _b) = body(resp).await;
        assert!(!h.contains_key(header::LAST_MODIFIED));

        // IMS on the epoch date must NOT 304 (condNone).
        let resp = get(
            &p,
            &[("If-Modified-Since", "Thu, 01 Jan 1970 00:00:00 GMT")],
        )
        .await;
        let (status, _h, _b) = body(resp).await;
        assert_eq!(status, 200);

        // If-Range date compare: t.Unix() == modtime.Unix() → Range kept.
        let resp = get(
            &p,
            &[
                ("Range", "bytes=0-1"),
                ("If-Range", "Thu, 01 Jan 1970 00:00:00 GMT"),
            ],
        )
        .await;
        let (status, h, b) = body(resp).await;
        assert_eq!(status, 206);
        assert_eq!(&b[..], b"01");
        assert_eq!(h[header::CONTENT_RANGE], "bytes 0-1/10");
    }

    /// Drive GET /files with a pre-built header map (obs-text values need
    /// HeaderValue::from_bytes; `from_str` rejects non-ASCII).
    async fn get_headers(path: &str, headers: HeaderMap) -> axum::response::Response {
        let config = Config::new();
        let mut params = HashMap::new();
        params.insert("path".to_string(), path.to_string());
        download(&config, params, headers).await
    }

    // ---- obs-text header bytes: garbage, not absent ------------------------

    #[tokio::test]
    async fn obs_text_range_is_garbage_416_not_absent_200() {
        let (_d, p) = tmp_file("r.bin", b"0123456789");
        let mut h = HeaderMap::new();
        h.insert(
            header::RANGE,
            header::HeaderValue::from_bytes(b"bytes=5-6\xFF").unwrap(),
        );
        let (status, _h, _b) = body(get_headers(&p, h).await).await;
        assert_eq!(status, 416);
    }

    #[tokio::test]
    async fn obs_text_inm_is_garbage_serve_not_ims_304() {
        let (_d, p) = tmp_file("c.txt", b"cache-me");
        let mut h = HeaderMap::new();
        h.insert(
            header::IF_NONE_MATCH,
            header::HeaderValue::from_bytes(b"\xFF").unwrap(),
        );
        h.insert(
            header::IF_MODIFIED_SINCE,
            "Sun, 06 Sep 2099 07:00:00 GMT".parse().unwrap(),
        );
        // Go: INM garbage → scan break → condTrue → serve; IMS is skipped,
        // so the fresh IMS date must NOT produce 304.
        let (status, _h, _b) = body(get_headers(&p, h).await).await;
        assert_eq!(status, 200);
    }

    #[tokio::test]
    async fn obs_text_if_range_drops_range() {
        let (_d, p) = tmp_file("r.bin", b"0123456789");
        let mut h = HeaderMap::new();
        h.insert(header::RANGE, "bytes=0-1".parse().unwrap());
        h.insert(
            header::IF_RANGE,
            header::HeaderValue::from_bytes(b"\xFF").unwrap(),
        );
        // Go: neither a valid etag nor a parseable date → condFalse → Range
        // dropped → full 200, never 206.
        let (status, _h, _b) = body(get_headers(&p, h).await).await;
        assert_eq!(status, 200);
    }

    #[tokio::test]
    async fn obs_text_accept_encoding_is_garbage_406() {
        let (_d, p) = tmp_file("r.bin", b"0123456789");
        let mut h = HeaderMap::new();
        h.insert(header::RANGE, "bytes=0-1".parse().unwrap());
        h.insert(
            header::ACCEPT_ENCODING,
            header::HeaderValue::from_bytes(b"identity;q=0,\xFF").unwrap(),
        );
        // Go encoding.go: the garbage token is neither identity nor a
        // supported encoding, identity is q=0-rejected → parse error
        // ("no acceptable encoding found") → 406 before Vary is set.
        let resp = get_headers(&p, h).await;
        let (status, hvary, b) = body(resp).await;
        assert_eq!(status, 406);
        assert_eq!(
            json_message(&b),
            "error parsing Accept-Encoding: no acceptable encoding found, supported: [gzip]"
        );
        assert!(!hvary.contains_key(header::VARY));
    }
}

/// POST /files shapes where upstream's rule and a "reasonable" reading differ.
/// POST /files shapes where upstream's rule and a "reasonable" reading differ.
/// Every case here was measured against the Go daemons before it was written
/// (see `docs/cube-envd/sdk-call-sites-issues-zh.md`): the `?path` query wins,
/// only parts named `file` are files, and an unsupported Content-Type is a 400
/// that never touches the body.
#[cfg(test)]
mod upload_semantics_tests {
    use axum::body::Body;
    use axum::http::{header, HeaderMap, StatusCode};
    use std::collections::HashMap;

    fn multipart_body(boundary: &str, parts: &[(&str, Option<&str>, &[u8])]) -> Vec<u8> {
        let mut body = Vec::new();
        for (name, filename, data) in parts {
            body.extend_from_slice(format!("--{boundary}\r\n").as_bytes());
            match filename {
                Some(filename) => body.extend_from_slice(
                    format!(
                        "Content-Disposition: form-data; name=\"{name}\"; filename=\"{filename}\"\r\n"
                    )
                    .as_bytes(),
                ),
                None => body.extend_from_slice(
                    format!("Content-Disposition: form-data; name=\"{name}\"\r\n").as_bytes(),
                ),
            }
            body.extend_from_slice(b"Content-Type: application/octet-stream\r\n\r\n");
            body.extend_from_slice(data);
            body.extend_from_slice(b"\r\n");
        }
        body.extend_from_slice(format!("--{boundary}--\r\n").as_bytes());
        body
    }

    /// A part with a `filename` but no `name` parameter: Go's `FormName()`
    /// returns `""` for an absent one, so upstream's `handlePart` skips it.
    fn multipart_body_unnamed(boundary: &str, filename: &str, data: &[u8]) -> Vec<u8> {
        let mut body = format!("--{boundary}\r\n").into_bytes();
        body.extend_from_slice(
            format!("Content-Disposition: form-data; filename=\"{filename}\"\r\n").as_bytes(),
        );
        body.extend_from_slice(b"Content-Type: application/octet-stream\r\n\r\n");
        body.extend_from_slice(data);
        body.extend_from_slice(format!("\r\n--{boundary}--\r\n").as_bytes());
        body
    }

    async fn post(
        params: &[(&str, &str)],
        content_type: &str,
        body: Vec<u8>,
    ) -> (StatusCode, String) {
        let mut headers = HeaderMap::new();
        headers.insert(header::CONTENT_TYPE, content_type.parse().unwrap());
        let params = params
            .iter()
            .map(|(k, v)| ((*k).to_string(), (*v).to_string()))
            .collect::<HashMap<_, _>>();
        let response = crate::filesystem::upload(
            &crate::platform::config::Config::new(),
            params,
            headers,
            Body::from(body),
        )
        .await;
        let (parts, body) = response.into_parts();
        let body = axum::body::to_bytes(body, 1 << 20).await.unwrap();
        (parts.status, String::from_utf8_lossy(&body).into_owned())
    }

    /// A part built with arbitrary header text, so a test can say exactly what
    /// the `Content-Disposition` looks like (`multipart_body` always writes the
    /// canonical lowercase form). `None` omits the header entirely.
    fn multipart_body_raw(boundary: &str, parts: &[(Option<&str>, &[u8])]) -> Vec<u8> {
        let mut body = Vec::new();
        for (disposition, data) in parts {
            body.extend_from_slice(format!("--{boundary}\r\n").as_bytes());
            if let Some(disposition) = disposition {
                body.extend_from_slice(
                    format!("Content-Disposition: {disposition}\r\n").as_bytes(),
                );
            }
            body.extend_from_slice(b"Content-Type: application/octet-stream\r\n\r\n");
            body.extend_from_slice(data);
            body.extend_from_slice(b"\r\n");
        }
        body.extend_from_slice(format!("--{boundary}--\r\n").as_bytes());
        body
    }

    /// Go's MIME parser treats *parameter names* case-insensitively, so
    /// `NAME="file"` is a file part upstream; `multer`'s accessor compared it
    /// case-sensitively and the upload was dropped with a 200 `[]`.
    #[tokio::test]
    async fn an_uppercase_name_parameter_is_still_a_file() {
        let dir = tempfile::tempdir().unwrap();
        let target = dir.path().join("upper.bin");
        let boundary = "X";
        let disposition = format!(
            "FORM-DATA; NAME=\"file\"; FILENAME=\"{}\"",
            target.display()
        );
        let body = multipart_body_raw(boundary, &[(Some(&disposition), b"payload")]);
        let (status, text) = post(
            &[],
            &format!("multipart/form-data; boundary={boundary}"),
            body,
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        assert!(text.contains("upper.bin"), "{text}");
        assert_eq!(std::fs::read(&target).unwrap(), b"payload");
    }

    /// The `filename` parameter name is case-insensitive too (Go lowercases the
    /// map keys), and its value is used verbatim.
    #[tokio::test]
    async fn an_uppercase_filename_parameter_is_the_fallback() {
        let dir = tempfile::tempdir().unwrap();
        let target = dir.path().join("upper-filename.bin");
        let boundary = "X";
        let disposition = format!(
            "form-data; name=\"file\"; FILENAME=\"{}\"",
            target.display()
        );
        let body = multipart_body_raw(boundary, &[(Some(&disposition), b"payload")]);
        let (status, text) = post(
            &[],
            &format!("multipart/form-data; boundary={boundary}"),
            body,
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        assert!(text.contains("upper-filename.bin"), "{text}");
        assert_eq!(std::fs::read(&target).unwrap(), b"payload");
    }

    /// `FormName()` returns "" unless the disposition type is `form-data`, so a
    /// part that carries a filename under `attachment` is a form field.
    #[tokio::test]
    async fn a_non_form_data_disposition_is_skipped() {
        let dir = tempfile::tempdir().unwrap();
        let target = dir.path().join("attachment.bin");
        let boundary = "X";
        let disposition = format!(
            "attachment; name=\"file\"; filename=\"{}\"",
            target.display()
        );
        let body = multipart_body_raw(boundary, &[(Some(&disposition), b"payload")]);
        let (status, text) = post(
            &[],
            &format!("multipart/form-data; boundary={boundary}"),
            body,
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        assert_eq!(text, "[]");
        assert!(!target.exists());
    }

    /// A part with no `Content-Disposition` at all is not `form-data` either.
    #[tokio::test]
    async fn a_part_without_a_content_disposition_is_skipped() {
        let boundary = "X";
        let body = multipart_body_raw(boundary, &[(None, b"payload")]);
        let (status, text) = post(
            &[],
            &format!("multipart/form-data; boundary={boundary}"),
            body,
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        assert_eq!(text, "[]");
    }

    #[tokio::test]
    async fn query_path_wins_over_the_part_filename() {
        let dir = tempfile::tempdir().unwrap();
        let query_target = dir.path().join("query-target.txt");
        let part_name = dir.path().join("part-name.txt");
        let boundary = "X";
        let body = multipart_body(
            boundary,
            &[("file", Some(part_name.to_str().unwrap()), b"payload")],
        );
        let (status, text) = post(
            &[("path", query_target.to_str().unwrap())],
            &format!("multipart/form-data; boundary={boundary}"),
            body,
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        assert!(text.contains("query-target.txt"), "{text}");
        assert_eq!(std::fs::read(&query_target).unwrap(), b"payload");
        assert!(
            !part_name.exists(),
            "the part filename must not be the target when ?path is present"
        );
    }

    #[tokio::test]
    async fn part_filename_is_the_fallback_without_a_query_path() {
        let dir = tempfile::tempdir().unwrap();
        let target = dir.path().join("from-part.bin");
        let boundary = "X";
        let body = multipart_body(
            boundary,
            &[("file", Some(target.to_str().unwrap()), b"payload")],
        );
        let (status, text) = post(
            &[],
            &format!("multipart/form-data; boundary={boundary}"),
            body,
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        assert!(text.contains("from-part.bin"), "{text}");
        assert_eq!(std::fs::read(&target).unwrap(), b"payload");
    }

    #[tokio::test]
    async fn parts_not_named_file_are_skipped() {
        let dir = tempfile::tempdir().unwrap();
        let kept = dir.path().join("kept.bin");
        let other_name = dir.path().join("other-name.bin");
        let no_name = dir.path().join("no-name.bin");
        let boundary = "X";
        let body = multipart_body(
            boundary,
            &[
                ("envd", Some(other_name.to_str().unwrap()), b"must-not-land"),
                ("file", Some(kept.to_str().unwrap()), b"kept"),
            ],
        );
        let (status, text) = post(
            &[],
            &format!("multipart/form-data; boundary={boundary}"),
            body,
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        assert!(text.contains("kept.bin"), "{text}");
        assert_eq!(std::fs::read(&kept).unwrap(), b"kept");
        assert!(
            !other_name.exists(),
            "a non-`file` field must not be written"
        );

        // Go's `FormName()` has no default, so a part carrying a filename but
        // no `name` parameter is a form field as well.
        let body = multipart_body_unnamed(boundary, no_name.to_str().unwrap(), b"must-not-land");
        let (status, _) = post(
            &[],
            &format!("multipart/form-data; boundary={boundary}"),
            body,
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        assert!(
            !no_name.exists(),
            "a part without a name must not be written"
        );
    }

    #[tokio::test]
    async fn multipart_without_a_file_part_is_an_empty_ok() {
        let boundary = "X";
        let body = multipart_body(boundary, &[("note", None, b"not a file")]);
        let (status, text) = post(
            &[],
            &format!("multipart/form-data; boundary={boundary}"),
            body,
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        assert_eq!(text, "[]");
    }

    #[tokio::test]
    async fn unsupported_content_type_is_rejected_and_writes_nothing() {
        let dir = tempfile::tempdir().unwrap();
        let target = dir.path().join("must-not-exist.txt");
        let (status, text) = post(
            &[("path", target.to_str().unwrap())],
            "application/x-www-form-urlencoded",
            b"text=NOT-A-FILE".to_vec(),
        )
        .await;
        assert_eq!(status, StatusCode::BAD_REQUEST);
        assert!(
            text.contains(
                "unsupported content type: application/x-www-form-urlencoded, expected multipart/form-data or application/octet-stream"
            ),
            "{text}"
        );
        assert!(
            !target.exists(),
            "a rejected upload must not create the file"
        );
    }

    #[tokio::test]
    async fn missing_content_type_is_rejected() {
        let (status, text) = post(&[], "", b"raw".to_vec()).await;
        assert_eq!(status, StatusCode::BAD_REQUEST);
        assert!(text.contains("unsupported content type"), "{text}");
    }

    #[tokio::test]
    async fn raw_upload_without_a_path_uses_the_upstream_message() {
        let (status, text) = post(&[], "application/octet-stream", b"raw".to_vec()).await;
        assert_eq!(status, StatusCode::BAD_REQUEST);
        assert!(
            text.contains("path query parameter is required for raw body upload"),
            "{text}"
        );
    }

    #[tokio::test]
    async fn a_repeated_multipart_path_is_rejected_and_keeps_the_first_write() {
        let dir = tempfile::tempdir().unwrap();
        let target = dir.path().join("same.bin");
        let boundary = "X";
        let body = multipart_body(
            boundary,
            &[
                ("file", Some("/ignored/first.bin"), b"first"),
                ("file", Some("/ignored/second.bin"), b"second"),
            ],
        );
        let (status, text) = post(
            &[("path", target.to_str().unwrap())],
            &format!("multipart/form-data; boundary={boundary}"),
            body,
        )
        .await;
        assert_eq!(status, StatusCode::BAD_REQUEST);
        assert!(
            text.contains("you cannot upload multiple files to the same path"),
            "{text}"
        );
        // Only one other path was uploaded, so upstream appends no clause.
        assert!(
            !text.contains("also the following files were uploaded"),
            "{text}"
        );
        assert_eq!(std::fs::read(&target).unwrap(), b"first");
    }

    /// With two *other* paths present, upstream appends
    /// `strings.Join(alreadyUploaded, ", ")` through `%v`: a bare comma-space
    /// list in upload order, no brackets.
    #[tokio::test]
    async fn a_repeated_multipart_path_lists_the_other_files_upstream_style() {
        let dir = tempfile::tempdir().unwrap();
        let a = dir.path().join("a.bin");
        let b = dir.path().join("b.bin");
        let c = dir.path().join("c.bin");
        let boundary = "X";
        let body = multipart_body(
            boundary,
            &[
                ("file", Some(a.to_str().unwrap()), b"a"),
                ("file", Some(b.to_str().unwrap()), b"b"),
                ("file", Some(c.to_str().unwrap()), b"c"),
                ("file", Some(a.to_str().unwrap()), b"again"),
            ],
        );
        let (status, text) = post(
            &[],
            &format!("multipart/form-data; boundary={boundary}"),
            body,
        )
        .await;
        assert_eq!(status, StatusCode::BAD_REQUEST);
        assert!(
            text.contains(&format!(
                "also the following files were uploaded: {}, {}",
                b.display(),
                c.display()
            )),
            "{text}"
        );
        // The first three parts were written; the fourth is rejected unwritten.
        assert_eq!(std::fs::read(&a).unwrap(), b"a");
        assert_eq!(std::fs::read(&b).unwrap(), b"b");
        assert_eq!(std::fs::read(&c).unwrap(), b"c");
    }

    #[tokio::test]
    async fn a_file_part_without_a_filename_and_without_a_path_is_a_400() {
        let boundary = "X";
        let body = multipart_body(boundary, &[("file", None, b"data")]);
        let (status, text) = post(
            &[],
            &format!("multipart/form-data; boundary={boundary}"),
            body,
        )
        .await;
        assert_eq!(status, StatusCode::BAD_REQUEST);
        assert!(
            text.contains("filename not found in Content-Disposition header"),
            "{text}"
        );
    }

    /// The media type is matched case-insensitively and independently of its
    /// parameters, like upstream's `mime.ParseMediaType` + switch.
    #[tokio::test]
    async fn content_type_is_matched_case_insensitively() {
        let dir = tempfile::tempdir().unwrap();
        let target = dir.path().join("upper.bin");
        let (status, _) = post(
            &[("path", target.to_str().unwrap())],
            "Application/Octet-Stream",
            b"upper".to_vec(),
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        assert_eq!(std::fs::read(&target).unwrap(), b"upper");
    }

    /// Any `multipart/*` subtype takes the multipart path, not just
    /// `multipart/form-data`.
    #[tokio::test]
    async fn other_multipart_subtypes_use_the_multipart_path() {
        let dir = tempfile::tempdir().unwrap();
        let target = dir.path().join("mixed.bin");
        let boundary = "X";
        let body = multipart_body(
            boundary,
            &[("file", Some(target.to_str().unwrap()), b"mixed")],
        );
        let (status, _) = post(&[], &format!("multipart/mixed; boundary={boundary}"), body).await;
        assert_eq!(status, StatusCode::OK);
        assert_eq!(std::fs::read(&target).unwrap(), b"mixed");
    }
}

/// Upstream rejects a directory target before opening the file
/// (`processFile`: `path is a directory: <path>`, 400). Without the pre-check
/// the EISDIR from `open` reached the caller as a 500.
#[cfg(test)]
mod upload_dir_target_tests {
    use axum::body::Body;
    use axum::http::{header, HeaderMap, StatusCode};
    use std::collections::HashMap;

    #[tokio::test]
    async fn a_directory_target_is_a_400_with_upstreams_message() {
        let dir = tempfile::tempdir().unwrap();
        let mut headers = HeaderMap::new();
        headers.insert(
            header::CONTENT_TYPE,
            "application/octet-stream".parse().unwrap(),
        );
        let params =
            HashMap::from([("path".to_string(), dir.path().to_str().unwrap().to_string())]);
        let response = crate::filesystem::upload(
            &crate::platform::config::Config::new(),
            params,
            headers,
            Body::from("x"),
        )
        .await;
        let (parts, body) = response.into_parts();
        let body = axum::body::to_bytes(body, 1 << 20).await.unwrap();
        assert_eq!(parts.status, StatusCode::BAD_REQUEST);
        assert!(
            String::from_utf8_lossy(&body)
                .contains(&format!("path is a directory: {}", dir.path().display())),
            "{}",
            String::from_utf8_lossy(&body)
        );
    }
}
