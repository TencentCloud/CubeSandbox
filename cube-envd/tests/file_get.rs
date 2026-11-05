// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

mod support;

#[tokio::test]
async fn identity_download_preserves_bytes_and_reports_metadata() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("hello.txt");
    let payload = b"hello, full file\n";
    std::fs::write(&path, payload).unwrap();
    let (port, server) = support::spawn_daemon().await;
    let response = reqwest::Client::new()
        .get(format!("http://127.0.0.1:{port}/files"))
        .query(&[("path", path.to_str().unwrap())])
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 200);
    assert_eq!(
        response.headers()["content-type"],
        "text/plain; charset=utf-8"
    );
    assert_eq!(
        response.headers()["content-disposition"],
        "inline; filename=hello.txt"
    );
    assert_eq!(response.headers()["vary"], "Accept-Encoding");
    assert_eq!(response.content_length(), Some(payload.len() as u64));
    for name in ["content-encoding", "etag"] {
        assert!(!response.headers().contains_key(name));
    }
    assert_eq!(response.headers()["accept-ranges"], "bytes");
    assert!(response.headers().contains_key("last-modified"));
    assert_eq!(response.bytes().await.unwrap().as_ref(), payload);
    server.abort();
}

#[tokio::test]
async fn gzip_negotiation_uses_oracle_quality_and_wildcard_semantics() {
    use std::io::Read;
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("payload.txt");
    let payload = b"full gzip text\n".repeat(10000);
    std::fs::write(&path, &payload).unwrap();
    let (port, server) = support::spawn_daemon().await;
    for (encoding, status, gzip) in [
        ("gzip", 200, true),
        ("GZip;Q=0.5", 200, true),
        ("identity;q=1,gzip;q=0.5", 200, false),
        ("gzip;q=1,identity;q=0.5", 200, true),
        ("*", 200, false),
        ("*;q=0, gzip", 200, true),
        ("*,identity;q=0", 200, true),
        ("br", 200, false),
        ("gzip;q=0", 200, false),
        ("*;q=0", 406, false),
        ("br,identity;q=0", 406, false),
        ("gzip;q=wrong", 200, true),
        ("gzip;q=1.1", 200, true),
        ("gzip;q=-1", 200, true),
        ("gzip;q=NaN", 200, true),
        ("gzip;q=0.1234", 200, true),
        ("gzip;level=1", 200, true),
        ("gzip;q=1;q=0", 200, false),
        ("g zip", 200, false),
    ] {
        let response = reqwest::Client::new()
            .get(format!("http://127.0.0.1:{port}/files"))
            .query(&[("path", path.to_str().unwrap())])
            .header("accept-encoding", encoding)
            .send()
            .await
            .unwrap();
        assert_eq!(response.status(), status, "{encoding}");
        if status != 200 {
            continue;
        }
        assert_eq!(response.headers()["vary"], "Accept-Encoding");
        assert_eq!(
            response.headers()["content-type"],
            "text/plain; charset=utf-8"
        );
        assert_eq!(
            response.headers().contains_key("content-encoding"),
            gzip,
            "{encoding}"
        );
        if gzip {
            assert_eq!(response.headers()["content-encoding"], "gzip");
        }
        let wire = response.bytes().await.unwrap();
        let mut decoded = Vec::new();
        if gzip {
            flate2::read::GzDecoder::new(wire.as_ref())
                .read_to_end(&mut decoded)
                .unwrap();
        } else {
            decoded.extend_from_slice(&wire);
        }
        assert_eq!(decoded, payload, "{encoding}");
    }
    server.abort();
}

#[tokio::test]
async fn mime_sniff_replays_prefix_and_disposition_encodes_the_basename() {
    use std::io::Read;
    let dir = tempfile::tempdir().unwrap();
    let (port, server) = support::spawn_daemon().await;
    for (name, prefix, mime, disposition) in [
        (
            "unknown",
            b"hello text\n".as_slice(),
            "text/plain; charset=utf-8",
            "inline; filename=unknown",
        ),
        (
            "binary",
            b"\x00\x01\x02".as_slice(),
            "application/octet-stream",
            "inline; filename=binary",
        ),
        (
            "pic",
            b"\x89PNG\r\n\x1a\n".as_slice(),
            "image/png",
            "inline; filename=pic",
        ),
        (
            "doc",
            b"  <!DOCTYPE HTML>".as_slice(),
            "text/html; charset=utf-8",
            "inline; filename=doc",
        ),
        (
            ".json",
            b"{}".as_slice(),
            "application/json",
            "inline; filename=.json",
        ),
        (
            "x.json",
            b"{}".as_slice(),
            "application/json",
            "inline; filename=x.json",
        ),
        (
            "a b.txt",
            b"txt".as_slice(),
            "text/plain; charset=utf-8",
            "inline; filename=\"a b.txt\"",
        ),
        (
            "中文.txt",
            b"txt".as_slice(),
            "text/plain; charset=utf-8",
            "inline; filename*=utf-8''%E4%B8%AD%E6%96%87.txt",
        ),
        (
            "a\"b\\c.txt",
            b"txt".as_slice(),
            "text/plain; charset=utf-8",
            "inline; filename=\"a\\\"b\\\\c.txt\"",
        ),
    ] {
        let path = dir.path().join(name);
        let mut payload = prefix.to_vec();
        payload.extend(vec![b'x'; 90000]);
        std::fs::write(&path, &payload).unwrap();
        for encoding in ["identity", "gzip"] {
            let response = reqwest::Client::new()
                .get(format!("http://127.0.0.1:{port}/files"))
                .query(&[("path", path.to_str().unwrap())])
                .header("accept-encoding", encoding)
                .send()
                .await
                .unwrap();
            assert_eq!(response.status(), 200, "{name}");
            assert_eq!(
                response.headers()["content-type"],
                if encoding == "gzip" && !name.contains('.') {
                    "application/octet-stream"
                } else {
                    mime
                },
                "{name}"
            );
            assert_eq!(
                response.headers()["content-disposition"],
                disposition,
                "{name}"
            );
            let wire = response.bytes().await.unwrap();
            let mut decoded = Vec::new();
            if encoding == "gzip" {
                flate2::read::GzDecoder::new(wire.as_ref())
                    .read_to_end(&mut decoded)
                    .unwrap();
            } else {
                decoded.extend_from_slice(&wire);
            }
            assert_eq!(decoded, payload);
        }
    }
    server.abort();
}

#[tokio::test]
async fn object_types_signatures_and_shared_path_resolution() {
    use std::os::unix::fs::symlink;
    let dir = tempfile::tempdir().unwrap();
    let file = dir.path().join("target");
    std::fs::write(&file, b"bound target").unwrap();
    symlink("target", dir.path().join("link")).unwrap();
    symlink("missing", dir.path().join("dangling")).unwrap();
    symlink("loop", dir.path().join("loop")).unwrap();
    symlink("/dev/null", dir.path().join("device-link")).unwrap();
    let fifo = std::ffi::CString::new(dir.path().join("fifo").to_str().unwrap()).unwrap();
    assert_eq!(unsafe { libc::mkfifo(fifo.as_ptr(), 0o600) }, 0);
    let _socket = std::os::unix::net::UnixListener::bind(dir.path().join("socket")).unwrap();
    let (port, server) = support::spawn_daemon().await;
    let client = reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(3))
        .build()
        .unwrap();
    let url = format!("http://127.0.0.1:{port}/files");
    for (path, status) in [
        (file.clone(), 200),
        (dir.path().join("link"), 200),
        (dir.path().to_owned(), 400),
        (dir.path().join("dangling"), 404),
        (dir.path().join("loop"), 500),
        (dir.path().join("socket"), 500),
        (dir.path().join("device-link"), 200),
        (std::path::PathBuf::from("/dev/zero"), 200),
    ] {
        let response = client
            .get(&url)
            .query(&[("path", path.to_str().unwrap())])
            .send()
            .await
            .unwrap();
        assert_eq!(response.status(), status, "{path:?}");
        if status == 200 {
            assert_eq!(
                response.bytes().await.unwrap().as_ref(),
                if path == file || path == dir.path().join("link") {
                    b"bound target".as_slice()
                } else {
                    b"".as_slice()
                }
            );
        }
    }
    for parameter in ["signature", "signature_expiration", "%73ignature"] {
        let response = client
            .get(format!("{url}?path=/definitely/missing&{parameter}="))
            .header("authorization", "invalid")
            .header("accept-encoding", "gzip;q=bad")
            .send()
            .await
            .unwrap();
        assert_eq!(
            response.status(),
            if parameter == "signature_expiration" {
                400
            } else {
                404
            }
        );
    }
    assert_eq!(client.head(&url).send().await.unwrap().status(), 405);
    for path in ["nul\0path", "/dev/null/child"] {
        let response = client
            .get(&url)
            .query(&[("path", path)])
            .send()
            .await
            .unwrap();
        assert_eq!(
            response.status(),
            if path.contains('\0') { 400 } else { 404 }
        );
    }
    assert_eq!(
        client
            .post(format!("http://127.0.0.1:{port}/init"))
            .json(&serde_json::json!({"defaultWorkdir": file}))
            .send()
            .await
            .unwrap()
            .status(),
        204
    );
    assert_eq!(
        client
            .get(&url)
            .send()
            .await
            .unwrap()
            .bytes()
            .await
            .unwrap(),
        "bound target"
    );
    let response = client
        .get(&url)
        .query(&[("path", file.to_str().unwrap())])
        .header("authorization", "Basic bm9uZXhpc3RlbnQtdXNlcjo=")
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 401);
    server.abort();
}

#[tokio::test]
async fn bound_download_survives_rename_unlink_replacement_and_link_retarget() {
    use std::os::unix::fs::symlink;
    let dir = tempfile::tempdir().unwrap();
    let target = dir.path().join("target");
    let link = dir.path().join("link");
    let payload = vec![b'a'; 16 * 1024 * 1024];
    std::fs::write(&target, &payload).unwrap();
    symlink("target", &link).unwrap();
    let (port, server) = support::spawn_daemon().await;
    let response = reqwest::Client::new()
        .get(format!("http://127.0.0.1:{port}/files"))
        .query(&[("path", link.to_str().unwrap())])
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 200);
    std::fs::rename(&target, dir.path().join("moved")).unwrap();
    std::fs::remove_file(dir.path().join("moved")).unwrap();
    std::fs::write(&target, b"replacement").unwrap();
    std::fs::remove_file(&link).unwrap();
    symlink("/dev/zero", &link).unwrap();
    assert_eq!(response.bytes().await.unwrap().as_ref(), payload);
    server.abort();
}

#[tokio::test]
async fn stalled_downloads_allow_independent_delivery_and_cancellation() {
    use std::io::{Read, Write};
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("large");
    // Incompressible bytes make gzip exert real socket backpressure too.
    let mut random = vec![0; 8 * 1024 * 1024];
    std::fs::File::open("/dev/urandom")
        .unwrap()
        .read_exact(&mut random)
        .unwrap();
    std::fs::File::create(&path)
        .unwrap()
        .write_all(&random)
        .unwrap();
    let (port, server) = support::spawn_daemon().await;
    let client = reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(5))
        .build()
        .unwrap();
    let url = format!("http://127.0.0.1:{port}/files");
    for encoding in ["identity", "gzip"] {
        let mut stalled = Vec::new();
        for _ in 0..8 {
            let response = client
                .get(&url)
                .query(&[("path", path.to_str().unwrap())])
                .header("accept-encoding", encoding)
                .send()
                .await
                .unwrap();
            assert_eq!(response.status(), 200);
            stalled.push(response);
        }
        let independent = client
            .get(&url)
            .query(&[("path", path.to_str().unwrap())])
            .send()
            .await
            .unwrap();
        assert_eq!(independent.status(), 200);
        assert_eq!(independent.bytes().await.unwrap().as_ref(), random);
        assert_eq!(
            client
                .get(format!("http://127.0.0.1:{port}/health"))
                .send()
                .await
                .unwrap()
                .status(),
            204
        );
        assert_eq!(
            client
                .post(format!("http://127.0.0.1:{port}/process.Process/List"))
                .header("connect-protocol-version", "1")
                .json(&serde_json::json!({}))
                .send()
                .await
                .unwrap()
                .status(),
            200
        );
        drop(stalled);
        let end = std::time::Instant::now() + std::time::Duration::from_secs(5);
        loop {
            let response = client
                .get(&url)
                .query(&[("path", path.to_str().unwrap())])
                .send()
                .await
                .unwrap();
            if response.status() == 200 {
                assert_eq!(response.bytes().await.unwrap().as_ref(), random);
                break;
            }
            assert!(
                std::time::Instant::now() < end,
                "fresh download failed after cancellation"
            );
            tokio::time::sleep(std::time::Duration::from_millis(20)).await;
        }
    }
    server.abort();
}

#[tokio::test]
async fn concurrent_truncate_is_a_transport_failure_and_overwrite_is_visible() {
    use std::os::unix::fs::FileExt;
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("mutable");
    let payload = vec![b'a'; 16 * 1024 * 1024];
    let (port, server) = support::spawn_daemon().await;
    let client = reqwest::Client::new();
    let url = format!("http://127.0.0.1:{port}/files");
    std::fs::write(&path, &payload).unwrap();
    let response = client
        .get(&url)
        .query(&[("path", path.to_str().unwrap())])
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 200);
    std::fs::OpenOptions::new()
        .write(true)
        .open(&path)
        .unwrap()
        .set_len(0)
        .unwrap();
    assert!(
        response.bytes().await.is_err(),
        "truncated body must not masquerade as complete"
    );
    std::fs::write(&path, &payload).unwrap();
    let response = client
        .get(&url)
        .query(&[("path", path.to_str().unwrap())])
        .send()
        .await
        .unwrap();
    let file = std::fs::OpenOptions::new().write(true).open(&path).unwrap();
    file.write_all_at(b"modified", (payload.len() - 8) as u64)
        .unwrap();
    file.write_all_at(b"appended", payload.len() as u64)
        .unwrap();
    let bytes = response.bytes().await.unwrap();
    assert_eq!(bytes.len(), payload.len());
    assert_eq!(&bytes[bytes.len() - 8..], b"modified");
    server.abort();
}

#[tokio::test]
async fn sdk_username_query_selects_the_user_before_path_resolution() {
    let (port, server) = support::spawn_daemon().await;
    let response = reqwest::Client::new()
        .get(format!("http://127.0.0.1:{port}/files"))
        .query(&[
            ("path", "/proc/version"),
            ("username", "nonexistent-file-user"),
        ])
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 401);
    server.abort();
}

#[tokio::test]
async fn fifo_identity_opens_then_reports_upstream_seek_error() {
    use std::io::Write;
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("fifo");
    let name = std::ffi::CString::new(path.to_str().unwrap()).unwrap();
    assert_eq!(unsafe { libc::mkfifo(name.as_ptr(), 0o600) }, 0);
    let (sent, received) = std::sync::mpsc::channel();
    let writer_path = path.clone();
    let writer = std::thread::spawn(move || {
        let mut file = std::fs::OpenOptions::new()
            .write(true)
            .open(writer_path)
            .unwrap();
        sent.send(()).unwrap();
        let _ = file.write_all(b"must not be downloaded");
    });
    let (port, server) = support::spawn_daemon().await;
    let response = reqwest::Client::new()
        .get(format!("http://127.0.0.1:{port}/files"))
        .query(&[("path", path.to_str().unwrap())])
        .send()
        .await
        .unwrap();
    assert!(
        received.try_recv().is_ok(),
        "GET must open the upstream FIFO"
    );
    writer.join().unwrap();
    assert_eq!(response.status(), 500);
    assert_eq!(response.text().await.unwrap(), "seeker can't seek\n");
    server.abort();
}
