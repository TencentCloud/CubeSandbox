// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

mod support;
use std::os::unix::fs::MetadataExt;

#[tokio::test]
async fn octet_upload_preserves_inode_and_creates_parents_and_dangling_target() {
    let dir = tempfile::tempdir().unwrap();
    let target = dir.path().join("existing");
    std::fs::write(&target, b"old longer content").unwrap();
    let inode = std::fs::metadata(&target).unwrap().ino();
    std::os::unix::fs::symlink("new/parents/file", dir.path().join("link")).unwrap();
    let (port, server) = support::spawn_daemon().await;
    let client = reqwest::Client::new();
    for path in [target.clone(), dir.path().join("link")] {
        let response = client
            .post(format!("http://127.0.0.1:{port}/files"))
            .query(&[("path", path.to_str().unwrap())])
            .header("content-type", "application/octet-stream")
            .body(b"\x00binary\xff".to_vec())
            .send()
            .await
            .unwrap();
        assert_eq!(response.status(), 200, "{}", response.text().await.unwrap());
        let entries: serde_json::Value = response.json().await.unwrap();
        assert_eq!(entries[0]["path"], path.to_str().unwrap());
        assert_eq!(entries[0]["type"], "file");
        let read = client
            .get(format!("http://127.0.0.1:{port}/files"))
            .query(&[("path", path.to_str().unwrap())])
            .send()
            .await
            .unwrap();
        assert_eq!(read.bytes().await.unwrap().as_ref(), b"\x00binary\xff");
    }
    assert_eq!(std::fs::metadata(target).unwrap().ino(), inode);
    assert_eq!(
        std::fs::metadata(dir.path().join("new/parents/file"))
            .unwrap()
            .mode()
            & 0o777,
        0o644
    );
    assert_eq!(
        std::fs::metadata(dir.path().join("new/parents"))
            .unwrap()
            .mode()
            & 0o777,
        0o755
    );
    server.abort();
}

#[tokio::test]
async fn gzip_validation_precedes_mutation_but_bad_trailer_keeps_written_content() {
    use std::io::Write;
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("target");
    let (port, server) = support::spawn_daemon().await;
    let client = reqwest::Client::new();
    let payload = b"gzip binary\0\xff".repeat(9000);
    let mut encoder = flate2::write::GzEncoder::new(Vec::new(), flate2::Compression::default());
    encoder.write_all(&payload).unwrap();
    let good = encoder.finish().unwrap();
    let mut corrupt = good.clone();
    let end = corrupt.len();
    corrupt[end - 8] ^= 1;
    for (encoding, body, expected, changed) in [
        ("gzip", good.clone(), 200, true),
        ("GZIP", good, 200, true),
        ("gzip", b"not gzip".to_vec(), 400, false),
        ("gzip", vec![0x1f, 0x8b, 8], 400, false),
        ("gzip, gzip", vec![], 400, false),
        ("gzip;level=1", vec![], 400, false),
        ("br", vec![], 400, false),
        ("gzip", corrupt, 500, true),
    ] {
        std::fs::write(&path, b"original").unwrap();
        let response = client
            .post(format!("http://127.0.0.1:{port}/files"))
            .query(&[("path", path.to_str().unwrap())])
            .header("content-type", "application/octet-stream")
            .header("content-encoding", encoding)
            .body(body)
            .send()
            .await
            .unwrap();
        assert_eq!(response.status(), expected, "{encoding}");
        assert!(
            std::fs::read(&path).unwrap()
                == if changed {
                    payload.clone()
                } else {
                    b"original".to_vec()
                },
            "content mismatch for {encoding}"
        );
    }
    server.abort();
}

fn multipart(parts: &[(&str, &str)]) -> Vec<u8> {
    let mut body = Vec::new();
    for (filename, data) in parts {
        body.extend_from_slice(format!("--boundary\r\nContent-Disposition: form-data; name=\"file\"; filename=\"{filename}\"\r\n\r\n{data}\r\n").as_bytes());
    }
    body.extend_from_slice(b"--boundary--\r\n");
    body
}

#[tokio::test]
async fn multipart_commits_in_wire_order_and_duplicate_does_not_overwrite() {
    let dir = tempfile::tempdir().unwrap();
    let first = dir.path().join("first");
    let duplicate = format!("{}/./first", dir.path().display());
    let later = dir.path().join("later");
    let (port, server) = support::spawn_daemon().await;
    let client = reqwest::Client::new();
    let response = client
        .post(format!("http://127.0.0.1:{port}/files"))
        .header("content-type", "multipart/form-data; boundary=boundary")
        .body(multipart(&[
            (first.to_str().unwrap(), "first data"),
            (&duplicate, "must not replace"),
            (later.to_str().unwrap(), "untouched"),
        ]))
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 400);
    assert_eq!(std::fs::read(&first).unwrap(), b"first data");
    assert!(!later.exists());
    let response = client
        .post(format!("http://127.0.0.1:{port}/files"))
        .header("content-type", "multipart/form-data; boundary=\"boundary\"")
        .body(multipart(&[
            (first.to_str().unwrap(), "new"),
            (later.to_str().unwrap(), "second"),
        ]))
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 200);
    let results: serde_json::Value = response.json().await.unwrap();
    assert_eq!(results[0]["path"], first.to_str().unwrap());
    assert_eq!(results[1]["path"], later.to_str().unwrap());
    assert_eq!(std::fs::read(later).unwrap(), b"second");
    server.abort();
}

async fn raw_upload(port: u16, path: &std::path::Path, extra: &str) -> tokio::net::TcpStream {
    use tokio::io::AsyncWriteExt;
    let mut stream = tokio::net::TcpStream::connect(("127.0.0.1", port))
        .await
        .unwrap();
    stream.write_all(format!("POST /files?path={} HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/octet-stream\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n{extra}\r\n", path.display()).as_bytes()).await.unwrap();
    stream
}

async fn wait_bytes(path: &std::path::Path, size: u64) {
    let end = std::time::Instant::now() + std::time::Duration::from_secs(5);
    loop {
        if std::fs::metadata(path).is_ok_and(|info| info.len() == size) {
            return;
        }
        assert!(
            std::time::Instant::now() < end,
            "file never reached size {size}: {path:?}"
        );
        tokio::time::sleep(std::time::Duration::from_millis(10)).await;
    }
}

#[tokio::test]
async fn bound_upload_survives_retarget_rename_unlink_and_replacement() {
    use std::io::Read;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("target");
    let link = dir.path().join("link");
    std::fs::write(&path, b"original").unwrap();
    std::os::unix::fs::symlink("target", &link).unwrap();
    let mut bound = std::fs::File::open(&path).unwrap();
    let (port, server) = support::spawn_daemon().await;
    let mut stream = raw_upload(port, &link, "").await;
    stream.write_all(b"3\r\none\r\n").await.unwrap();
    wait_bytes(&path, 3).await;
    std::fs::rename(&path, dir.path().join("moved")).unwrap();
    std::fs::remove_file(dir.path().join("moved")).unwrap();
    std::fs::write(&path, b"replacement").unwrap();
    std::fs::remove_file(&link).unwrap();
    std::os::unix::fs::symlink("/dev/null", &link).unwrap();
    stream.write_all(b"3\r\ntwo\r\n0\r\n\r\n").await.unwrap();
    let mut response = String::new();
    stream.read_to_string(&mut response).await.unwrap();
    assert!(response.starts_with("HTTP/1.1 200"), "{response}");
    let mut content = Vec::new();
    bound.read_to_end(&mut content).unwrap();
    assert_eq!(content, b"onetwo");
    assert_eq!(std::fs::read(path).unwrap(), b"replacement");
    server.abort();
}

#[tokio::test]
async fn slow_uploads_allow_more_requests_and_disconnect_keeps_only_the_prefix() {
    use tokio::io::AsyncWriteExt;
    let dir = tempfile::tempdir().unwrap();
    let (port, server) = support::spawn_daemon().await;
    let client = reqwest::Client::new();
    let mut streams = Vec::new();
    for index in 0..8 {
        let path = dir.path().join(index.to_string());
        let mut stream = raw_upload(port, &path, "").await;
        stream.write_all(b"3\r\nabc\r\n").await.unwrap();
        wait_bytes(&path, 3).await;
        streams.push(stream);
    }
    let ninth = dir.path().join("ninth");
    assert_eq!(
        client
            .post(format!("http://127.0.0.1:{port}/files"))
            .query(&[("path", ninth.to_str().unwrap())])
            .header("content-type", "application/octet-stream")
            .body("x")
            .send()
            .await
            .unwrap()
            .status(),
        200
    );
    assert_eq!(std::fs::read(&ninth).unwrap(), b"x");
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
    drop(streams);
    let end = std::time::Instant::now() + std::time::Duration::from_secs(5);
    loop {
        let response = client
            .get(format!("http://127.0.0.1:{port}/files"))
            .query(&[("path", dir.path().join("0").to_str().unwrap())])
            .send()
            .await
            .unwrap();
        if response.status() == 200 {
            break;
        }
        assert!(std::time::Instant::now() < end);
        tokio::time::sleep(std::time::Duration::from_millis(10)).await;
    }
    for index in 0..8 {
        assert_eq!(
            std::fs::read(dir.path().join(index.to_string())).unwrap(),
            b"abc"
        );
    }
    let before = dir.path().join("before");
    let stream = raw_upload(port, &before, "Content-Encoding: gzip\r\n").await;
    tokio::time::sleep(std::time::Duration::from_millis(30)).await;
    drop(stream);
    tokio::time::sleep(std::time::Duration::from_millis(30)).await;
    assert!(!before.exists());
    server.abort();
}

#[tokio::test]
async fn multipart_override_aliases_nonfiles_and_partial_parser_failure() {
    let dir = tempfile::tempdir().unwrap();
    let first = dir.path().join("first");
    let later = dir.path().join("later");
    let (port, server) = support::spawn_daemon().await;
    let client = reqwest::Client::new();
    let url = format!("http://127.0.0.1:{port}/files");
    let response = client
        .post(&url)
        .query(&[("path", first.to_str().unwrap())])
        .header("content-type", "multipart/form-data; boundary=boundary")
        .body(multipart(&[
            ("ignored", "first"),
            ("different", "duplicate"),
        ]))
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 400);
    assert_eq!(std::fs::read(&first).unwrap(), b"first");
    let alias = dir.path().join("alias");
    std::fs::hard_link(&first, &alias).unwrap();
    let response = client
        .post(&url)
        .header("content-type", "multipart/form-data; boundary=boundary")
        .body(multipart(&[
            (first.to_str().unwrap(), "one"),
            (alias.to_str().unwrap(), "two"),
        ]))
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 200);
    assert_eq!(std::fs::read(&first).unwrap(), b"two");
    let mut body =
        b"--boundary\r\nContent-Disposition: form-data; name=\"ignored\"\r\n\r\nignored value\r\n"
            .to_vec();
    body.extend(multipart(&[
        (first.to_str().unwrap(), "done"),
        (later.to_str().unwrap(), &"x".repeat(100000)),
    ]));
    body.truncate(body.len() - 16); // malformed final boundary; earlier commit remains.
    let response = client
        .post(&url)
        .header("content-type", "multipart/form-data; boundary=boundary")
        .body(body)
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 400);
    assert_eq!(std::fs::read(first).unwrap(), b"done");
    let prefix = std::fs::read(later).unwrap();
    assert!(
        !prefix.is_empty() && prefix.len() <= 100000 && prefix.iter().all(|byte| *byte == b'x')
    );
    server.abort();
}

#[tokio::test]
async fn types_signatures_compose_and_invalid_paths_return_errors() {
    let dir = tempfile::tempdir().unwrap();
    let _socket = std::os::unix::net::UnixListener::bind(dir.path().join("socket")).unwrap();
    let (port, server) = support::spawn_daemon().await;
    let client = reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(3))
        .build()
        .unwrap();
    for path in [
        dir.path().to_str().unwrap(),
        "",
        "/",
        "a\0b",
        dir.path().join("socket").to_str().unwrap(),
    ] {
        let response = client
            .post(format!("http://127.0.0.1:{port}/files"))
            .query(&[("path", path)])
            .header("content-type", "application/octet-stream")
            .body("must not write")
            .send()
            .await
            .unwrap();
        assert_eq!(
            response.status(),
            if path.ends_with("/socket") { 500 } else { 400 },
            "{path}"
        );
    }
    let missing = dir.path().join("no/creation");
    for suffix in ["&signature=", "&signature_expiration=", "&%73ignature=x"] {
        let response = client
            .post(format!(
                "http://127.0.0.1:{port}/files?path={}{}",
                missing.display(),
                suffix
            ))
            .header("content-encoding", "invalid")
            .body("bad")
            .send()
            .await
            .unwrap();
        assert_eq!(response.status(), 400);
    }
    assert_eq!(
        client
            .post(format!(
                "http://127.0.0.1:{port}/files/compose?path={}",
                missing.display()
            ))
            .body("bad")
            .send()
            .await
            .unwrap()
            .status(),
        400
    );
    assert!(!dir.path().join("no").exists());
    server.abort();
}

#[tokio::test]
async fn uploads_stream_beyond_previous_file_and_body_limits() {
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("target");
    let (port, server) = support::spawn_daemon().await;
    let client = reqwest::Client::new();
    let url = format!("http://127.0.0.1:{port}/files");
    let payload = vec![b'x'; 65 * 1024 * 1024];
    for format in ["raw", "gzip", "multipart", "chunked"] {
        if format == "chunked" {
            let mut stream = raw_upload(port, &path, "").await;
            for chunk in payload.chunks(32768) {
                stream.write_all(b"8000\r\n").await.unwrap();
                stream.write_all(chunk).await.unwrap();
                stream.write_all(b"\r\n").await.unwrap();
            }
            stream.write_all(b"0\r\n\r\n").await.unwrap();
            let mut response = Vec::new();
            stream.read_to_end(&mut response).await.unwrap();
            assert!(response.starts_with(b"HTTP/1.1 200"));
        } else {
            let mut request = client.post(&url).query(&[("path", path.to_str().unwrap())]);
            let body = match format {
                "gzip" => {
                    request = request
                        .header("content-type", "application/octet-stream")
                        .header("content-encoding", "gzip");
                    let mut encoder =
                        flate2::write::GzEncoder::new(Vec::new(), flate2::Compression::fast());
                    std::io::Write::write_all(&mut encoder, &payload).unwrap();
                    encoder.finish().unwrap()
                }
                "multipart" => {
                    request =
                        request.header("content-type", "multipart/form-data; boundary=boundary");
                    multipart(&[("target", std::str::from_utf8(&payload).unwrap())])
                }
                _ => {
                    request = request.header("content-type", "application/octet-stream");
                    payload.clone()
                }
            };
            let response = request.body(body).send().await.unwrap();
            assert_eq!(response.status(), 200, "{format}");
        }
        let response = client
            .get(&url)
            .query(&[("path", path.to_str().unwrap())])
            .send()
            .await
            .unwrap();
        assert_eq!(
            response.bytes().await.unwrap().as_ref(),
            payload.as_slice(),
            "{format}"
        );
    }
    server.abort();
}

#[tokio::test]
async fn malformed_closing_boundary_is_not_a_successful_upload() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("file");
    let (port, server) = support::spawn_daemon().await;
    let mut body = multipart(&[(path.to_str().unwrap(), "content")]);
    body.truncate(body.len() - 2);
    body.extend_from_slice(b"junk\r\n");
    let response = reqwest::Client::new()
        .post(format!("http://127.0.0.1:{port}/files"))
        .header("content-type", "multipart/form-data; boundary=boundary")
        .body(body)
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 400);
    assert_eq!(std::fs::read(path).unwrap(), b"content");
    server.abort();
}

#[tokio::test]
async fn multipart_budgets_and_symlink_parent_walk_are_not_lexically_collapsed() {
    let dir = tempfile::tempdir().unwrap();
    std::fs::create_dir_all(dir.path().join("real/deep")).unwrap();
    std::os::unix::fs::symlink("real/deep", dir.path().join("link")).unwrap();
    let through = format!("{}/link/../target", dir.path().display());
    let lexical = dir.path().join("target");
    let (port, server) = support::spawn_daemon().await;
    let client = reqwest::Client::new();
    let url = format!("http://127.0.0.1:{port}/files");
    let response = client
        .post(&url)
        .header("content-type", "multipart/form-data; boundary=boundary")
        .body(multipart(&[
            (&through, "physical parent"),
            (lexical.to_str().unwrap(), "lexical parent"),
        ]))
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 200);
    assert_eq!(
        std::fs::read(dir.path().join("real/target")).unwrap(),
        b"physical parent"
    );
    assert_eq!(std::fs::read(&lexical).unwrap(), b"lexical parent");
    let oversized = format!("--boundary\r\nX-Long: {}\r\nContent-Disposition: form-data; filename=\"{}\"\r\n\r\nbad\r\n--boundary--\r\n", "x".repeat(16384), lexical.display());
    assert_eq!(
        client
            .post(&url)
            .header("content-type", "multipart/form-data; boundary=boundary")
            .body(oversized)
            .send()
            .await
            .unwrap()
            .status(),
        413
    );
    assert_eq!(std::fs::read(&lexical).unwrap(), b"lexical parent");
    let mut body =
        b"--boundary\r\nContent-Disposition: form-data; name=x\r\n\r\nignored\r\n".repeat(1000);
    body.extend(multipart(&[(
        lexical.to_str().unwrap(),
        "must not overwrite",
    )]));
    assert_eq!(
        client
            .post(&url)
            .header("content-type", "multipart/form-data; boundary=boundary")
            .body(body)
            .send()
            .await
            .unwrap()
            .status(),
        413
    );
    assert_eq!(std::fs::read(&lexical).unwrap(), b"lexical parent");
    // A single HTTP gzip coding can wrap multipart and multiple gzip members.
    let body = multipart(&[(lexical.to_str().unwrap(), "gzip multipart")]);
    let mut encoded = Vec::new();
    for half in body.chunks(body.len() / 2 + 1) {
        let mut encoder = flate2::write::GzEncoder::new(Vec::new(), flate2::Compression::default());
        std::io::Write::write_all(&mut encoder, half).unwrap();
        encoded.extend(encoder.finish().unwrap());
    }
    assert_eq!(
        client
            .post(&url)
            .header("content-type", "multipart/form-data; boundary=boundary")
            .header("content-encoding", "gzip")
            .body(encoded)
            .send()
            .await
            .unwrap()
            .status(),
        200
    );
    assert_eq!(std::fs::read(lexical).unwrap(), b"gzip multipart");
    server.abort();
}

#[tokio::test]
async fn gzip_header_metadata_is_bounded_before_mutation() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("file");
    std::fs::write(&path, b"original").unwrap();
    let (port, server) = support::spawn_daemon().await;
    let mut encoded = vec![0x1f, 0x8b, 8, 8, 0, 0, 0, 0, 0, 255];
    encoded.extend(vec![b'a'; 16384]);
    let response = reqwest::Client::new()
        .post(format!("http://127.0.0.1:{port}/files"))
        .query(&[("path", path.to_str().unwrap())])
        .header("content-type", "application/octet-stream")
        .header("content-encoding", "gzip")
        .body(encoded)
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 413);
    assert_eq!(std::fs::read(path).unwrap(), b"original");
    server.abort();
}

#[tokio::test]
async fn concurrent_uploads_progress_on_the_same_inode_without_a_path_lock() {
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("file");
    std::fs::write(&path, b"original").unwrap();
    let inode = std::fs::metadata(&path).unwrap().ino();
    let (port, server) = support::spawn_daemon().await;
    let mut first = raw_upload(port, &path, "").await;
    first.write_all(b"3\r\none\r\n").await.unwrap();
    wait_bytes(&path, 3).await;
    let mut second = raw_upload(port, &path, "").await;
    second.write_all(b"2\r\nab\r\n").await.unwrap();
    wait_bytes(&path, 2).await;
    first.write_all(b"3\r\ntwo\r\n0\r\n\r\n").await.unwrap();
    second.write_all(b"3\r\ncde\r\n0\r\n\r\n").await.unwrap();
    for mut stream in [first, second] {
        let mut response = String::new();
        stream.read_to_string(&mut response).await.unwrap();
        assert!(response.starts_with("HTTP/1.1 200"), "{response}");
    }
    assert_eq!(std::fs::metadata(path).unwrap().ino(), inode);
    // Both bound writers completed; deliberately no complete-payload/final-winner assertion.
    server.abort();
}

#[tokio::test]
async fn multipart_transport_padding_does_not_swallow_later_files() {
    let dir = tempfile::tempdir().unwrap();
    let first = dir.path().join("first");
    let second = dir.path().join("second");
    let body = format!("--boundary \t\r\nContent-Disposition: form-data; name=\"file\"; filename=\"{}\"\r\n\r\none\r\n--boundary \t\r\nContent-Disposition: form-data; name=\"file\"; filename=\"{}\"\r\n\r\ntwo\r\n--boundary--\r\n", first.display(), second.display());
    let (port, server) = support::spawn_daemon().await;
    let response = reqwest::Client::new()
        .post(format!("http://127.0.0.1:{port}/files"))
        .header("content-type", "multipart/form-data; boundary=boundary")
        .body(body)
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 200);
    let entries: serde_json::Value = response.json().await.unwrap();
    assert_eq!(entries.as_array().unwrap().len(), 2);
    assert_eq!(std::fs::read(first).unwrap(), b"one");
    assert_eq!(std::fs::read(second).unwrap(), b"two");
    server.abort();
}

#[tokio::test]
async fn proc_fd_magic_link_writes_the_unlinked_object_not_readlink_text() {
    use std::io::Read;
    use std::os::fd::AsRawFd;
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("unlinked");
    std::fs::write(&path, b"original").unwrap();
    let mut file = std::fs::File::open(&path).unwrap();
    std::fs::remove_file(&path).unwrap();
    // The file belongs to this test process, while requests run in a separate daemon.
    let magic = format!("/proc/{}/fd/{}", std::process::id(), file.as_raw_fd());
    let (port, server) = support::spawn_daemon().await;
    let response = reqwest::Client::new()
        .post(format!("http://127.0.0.1:{port}/files"))
        .query(&[("path", &magic)])
        .header("content-type", "application/octet-stream")
        .body("updated")
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 200);
    let mut data = Vec::new();
    file.read_to_end(&mut data).unwrap();
    assert_eq!(data, b"updated");
    assert!(!dir.path().join("unlinked (deleted)").exists());
    server.abort();
}
