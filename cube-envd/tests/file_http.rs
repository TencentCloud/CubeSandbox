// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

mod support;

#[tokio::test]
async fn single_range_returns_only_the_requested_bytes_and_metadata() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("file.txt");
    std::fs::write(&path, b"0123456789").unwrap();
    let (port, server) = support::spawn_daemon().await;
    let response = reqwest::Client::new()
        .get(format!("http://127.0.0.1:{port}/files"))
        .query(&[("path", path.to_str().unwrap())])
        .header("range", "bytes=1-3")
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 206);
    assert_eq!(response.headers()["content-range"], "bytes 1-3/10");
    assert_eq!(response.headers()["accept-ranges"], "bytes");
    assert_eq!(response.headers()["content-length"], "3");
    assert!(response.headers().contains_key("last-modified"));
    assert!(!response.headers().contains_key("etag"));
    assert_eq!(response.bytes().await.unwrap(), "123");
    server.abort();
}

#[tokio::test]
async fn range_boundaries_and_multipart_follow_the_upstream_response() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("file.txt");
    std::fs::write(&path, b"0123456789").unwrap();
    let (port, server) = support::spawn_daemon().await;
    let client = reqwest::Client::new();
    for (range, status, content_range, body) in [
        ("bytes=3-", 206, Some("bytes 3-9/10"), "3456789"),
        ("bytes=-3", 206, Some("bytes 7-9/10"), "789"),
        ("bytes=-0", 206, Some("bytes 10-9/10"), ""),
        (
            "bytes=99-x",
            416,
            Some("bytes */10"),
            "invalid range: failed to overlap\n",
        ),
        ("bytes=99-,1-2", 206, Some("bytes 1-2/10"), "12"),
        ("bytes=2-1", 416, None, "invalid range\n"),
        ("bytes=0-99", 206, Some("bytes 0-9/10"), "0123456789"),
        ("bytes=0-9,0-9", 200, None, "0123456789"),
        ("bytes=,", 200, None, "0123456789"),
        ("bytes=+1-+3", 206, Some("bytes 1-3/10"), "123"),
        ("Bytes=1-3", 416, None, "invalid range\n"),
        ("bytes=1-3,", 206, Some("bytes 1-3/10"), "123"),
        ("bytes=9223372036854775808-", 416, None, "invalid range\n"),
    ] {
        let r = client
            .get(format!("http://127.0.0.1:{port}/files"))
            .query(&[("path", path.to_str().unwrap())])
            .header("range", range)
            .send()
            .await
            .unwrap();
        assert_eq!(r.status(), status, "{range}");
        assert_eq!(
            r.headers()
                .get("content-range")
                .map(|v| v.to_str().unwrap()),
            content_range,
            "{range}"
        );
        assert_eq!(r.headers().contains_key("last-modified"), status != 416);
        assert_eq!(r.headers().contains_key("accept-ranges"), status != 416);
        if status == 416 {
            assert_eq!(r.headers()["x-content-type-options"], "nosniff");
        }
        assert_eq!(r.text().await.unwrap(), body, "{range}");
    }
    let r = client
        .get(format!("http://127.0.0.1:{port}/files"))
        .query(&[("path", path.to_str().unwrap())])
        .header("range", "bytes=0-1,5-7")
        .send()
        .await
        .unwrap();
    assert_eq!(r.status(), 206);
    let boundary = r.headers()["content-type"]
        .to_str()
        .unwrap()
        .strip_prefix("multipart/byteranges; boundary=")
        .unwrap()
        .to_owned();
    let length: usize = r.headers()["content-length"]
        .to_str()
        .unwrap()
        .parse()
        .unwrap();
    let body = r.text().await.unwrap();
    assert_eq!(body.len(), length);
    assert_eq!(body.replace(&boundary,"BOUNDARY"), "--BOUNDARY\r\nContent-Range: bytes 0-1/10\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n01\r\n--BOUNDARY\r\nContent-Range: bytes 5-7/10\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n567\r\n--BOUNDARY--\r\n");
    std::fs::write(&path, b"").unwrap();
    for (range, status, body) in [
        ("bytes=0-", 200, ""),
        ("bytes=-1", 206, ""),
        ("bad", 416, "invalid range\n"),
    ] {
        let r = client
            .get(format!("http://127.0.0.1:{port}/files"))
            .query(&[("path", path.to_str().unwrap())])
            .header("range", range)
            .send()
            .await
            .unwrap();
        assert_eq!(r.status(), status, "empty {range}");
        assert_eq!(r.text().await.unwrap(), body);
    }
    server.abort();
}

#[tokio::test]
async fn preconditions_precede_ranges_and_if_range_selects_the_representation() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("file.txt");
    std::fs::write(&path, b"0123456789").unwrap();
    std::fs::File::open(&path)
        .unwrap()
        .set_modified(std::time::UNIX_EPOCH + std::time::Duration::from_secs(1577934245))
        .unwrap();
    let (port, server) = support::spawn_daemon().await;
    let client = reqwest::Client::new();
    for (headers, status, body) in [
        (
            vec![("if-modified-since", "Thu, 02 Jan 2020 03:04:05 GMT")],
            304,
            "",
        ),
        (
            vec![("if-modified-since", "Wed, 02 Jan 2020 03:04:05 GMT")],
            304,
            "",
        ),
        (
            vec![("if-modified-since", "Thursday, 02-Jan-20 03:04:05 GMT")],
            304,
            "",
        ),
        (
            vec![("if-modified-since", "Thu Jan  2 03:04:05 2020")],
            304,
            "",
        ),
        (
            vec![("if-unmodified-since", "Thu, 01 Jan 1970 00:00:00 GMT")],
            412,
            "",
        ),
        (
            vec![
                ("if-match", "*"),
                ("if-unmodified-since", "Thu, 01 Jan 1970 00:00:00 GMT"),
            ],
            200,
            "0123456789",
        ),
        (
            vec![
                ("if-none-match", "\"x\""),
                ("if-modified-since", "Thu, 02 Jan 2020 03:04:05 GMT"),
            ],
            200,
            "0123456789",
        ),
        (vec![("if-match", "\"x\""), ("if-none-match", "*")], 412, ""),
        (
            vec![
                ("if-range", "Thu, 02 Jan 2020 03:04:05 GMT"),
                ("range", "bytes=1-2"),
            ],
            206,
            "12",
        ),
        (
            vec![("if-range", "\"x\""), ("range", "bad")],
            200,
            "0123456789",
        ),
        (vec![("if-none-match", "garbage,*")], 200, "0123456789"),
        (vec![("if-match", ", \"x\", *trailing")], 200, "0123456789"),
        (vec![("if-none-match", "*"), ("range", "bad")], 304, ""),
        (vec![("if-match", "bad"), ("range", "bad")], 412, ""),
    ] {
        let mut request = client
            .get(format!("http://127.0.0.1:{port}/files"))
            .query(&[("path", path.to_str().unwrap())]);
        for (k, v) in headers {
            request = request.header(k, v);
        }
        let r = request.send().await.unwrap();
        assert_eq!(r.status(), status);
        assert_eq!(
            r.headers()["last-modified"],
            "Thu, 02 Jan 2020 03:04:05 GMT"
        );
        assert!(!r.headers().contains_key("etag"));
        if status == 304 || status == 412 {
            assert!(!r.headers().contains_key("content-type"));
            assert!(!r.headers().contains_key("accept-ranges"));
        }
        assert_eq!(r.text().await.unwrap(), body);
    }
    server.abort();
}

#[tokio::test]
async fn encoding_fallback_and_permissive_quality_follow_the_file_handler() {
    use std::io::Read;
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("unknown");
    std::fs::write(&path, b"0123456789").unwrap();
    let (port, server) = support::spawn_daemon().await;
    let client = reqwest::Client::new();
    for (encoding,extra,status,gzip,body) in [
        ("gzip",Some(("range","bytes=1-3")),206,false,"123"),
        ("gzip",Some(("if-match","\"missing\"")),200,true,"0123456789"),
        ("gzip;q=wrong",None,200,true,"0123456789"),
        ("gzip;q=-1",None,200,true,"0123456789"),
        ("gzip;q=1;q=0",None,200,false,"0123456789"),
        ("gzip;level=1",None,200,true,"0123456789"),
        ("g zip",None,200,false,"0123456789"),
        ("gzip,identity;q=0",Some(("range","bytes=1-3")),406,false,"{\"code\":406,\"message\":\"identity encoding not acceptable for Range or conditional request\"}\n"),
        ("*;q=0",None,406,false,"{\"code\":406,\"message\":\"error parsing Accept-Encoding: no acceptable encoding found, supported: [gzip]\"}\n"),
    ] {
        let mut request=client.get(format!("http://127.0.0.1:{port}/files")).query(&[("path",path.to_str().unwrap())]).header("accept-encoding",encoding);
        if let Some((k,v))=extra { request=request.header(k,v); }
        let r=request.send().await.unwrap();
        assert_eq!(r.status(),status,"{encoding} {extra:?}");
        assert_eq!(r.headers().contains_key("content-encoding"),gzip);
        if gzip { assert_eq!(r.headers()["content-type"],"application/octet-stream"); }
        if status==406 {
            assert_eq!(r.headers()["content-type"],"application/json; charset=utf-8");
            assert_eq!(r.headers()["x-content-type-options"],"nosniff");
        }
        let bytes=r.bytes().await.unwrap();
        let mut decoded=Vec::new();
        if gzip { flate2::read::GzDecoder::new(bytes.as_ref()).read_to_end(&mut decoded).unwrap(); } else { decoded.extend_from_slice(&bytes); }
        assert_eq!(decoded,body.as_bytes());
    }
    server.abort();
}

#[tokio::test]
async fn file_method_and_lookup_errors_preserve_status_headers_and_priority() {
    let dir = tempfile::tempdir().unwrap();
    let missing = dir.path().join("missing");
    let (port, server) = support::spawn_daemon().await;
    let client = reqwest::Client::new();
    for method in [
        reqwest::Method::HEAD,
        reqwest::Method::PUT,
        reqwest::Method::DELETE,
    ] {
        let head = method == reqwest::Method::HEAD;
        let r = client
            .request(
                method,
                format!("http://127.0.0.1:{port}/files?path=/missing"),
            )
            .header("accept-encoding", "*;q=0")
            .send()
            .await
            .unwrap();
        assert_eq!(r.status(), 405);
        let allow: Vec<_> = r
            .headers()
            .get_all("allow")
            .iter()
            .map(|v| v.to_str().unwrap())
            .collect();
        assert_eq!(allow, vec!["GET", "POST"]);
        if head {
            assert!(!r.headers().contains_key("content-length"));
        }
        assert!(!r.headers().contains_key("cache-control"));
        assert!(!r.headers().contains_key("content-type"));
        assert!(r.bytes().await.unwrap().is_empty());
    }
    for (path, status, message) in [
        (
            missing.as_path(),
            404,
            format!("path '{}' does not exist", missing.display()),
        ),
        (
            dir.path(),
            400,
            format!("path '{}' is a directory", dir.path().display()),
        ),
    ] {
        let r = client
            .get(format!("http://127.0.0.1:{port}/files"))
            .query(&[("path", path.to_str().unwrap())])
            .header("accept-encoding", "*;q=0")
            .send()
            .await
            .unwrap();
        assert_eq!(r.status(), status);
        assert_eq!(
            r.headers()["content-type"],
            "application/json; charset=utf-8"
        );
        assert_eq!(r.headers()["x-content-type-options"], "nosniff");
        assert!(!r.headers().contains_key("cache-control"));
        assert_eq!(
            r.text().await.unwrap(),
            format!("{}\n", serde_json::json!({"code":status,"message":message}))
        );
    }
    server.abort();
}

#[tokio::test]
async fn upload_dispatch_and_multipart_fields_match_the_public_response() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("target");
    let (port, server) = support::spawn_daemon().await;
    let client = reqwest::Client::new();
    for (typ,coding,body,status,expected) in [
        ("text/plain","identity","x",400,"{\"code\":400,\"message\":\"unsupported content type: text/plain, expected multipart/form-data or application/octet-stream\"}\n"),
        ("application/octet-stream","br","x",400,"{\"code\":400,\"message\":\"error decompressing request body: unsupported Content-Encoding: br, supported: [gzip]\"}\n"),
        ("multipart/form-data","identity","x",500,"{\"code\":500,\"message\":\"error parsing multipart form: no multipart boundary param in Content-Type\"}\n"),
        ("multipart/mixed; boundary=B","identity","--B--\r\n",200,"[]"),
        ("multipart/form-data; boundary=B","identity","--B\r\nContent-Disposition: form-data; name=\"other\"; filename=\"ignored\"\r\n\r\nx\r\n--B--\r\n",200,"[]"),
    ] {
        let r=client.post(format!("http://127.0.0.1:{port}/files")).query(&[("path",path.to_str().unwrap())]).header("content-type",typ).header("content-encoding",coding).body(body).send().await.unwrap();
        assert_eq!(r.status(),status,"{typ} {coding}");
        assert_eq!(r.headers()["content-type"],if status==200 {"text/plain; charset=utf-8"} else {"application/json; charset=utf-8"});
        assert_eq!(r.text().await.unwrap(),expected);
    }
    for (typ,body) in [
        ("application/octet-stream; x=y; x=y","payload"),
        ("multipart/form-data; boundary=B","--B\r\nContent-Disposition: form-data; name=\"file\"; filename=\"\"\r\n\r\npayload\r\n--B--\r\n")
    ] {
        let r=client.post(format!("http://127.0.0.1:{port}/files")).query(&[("path",path.to_str().unwrap())]).header("content-type",typ).header("content-encoding","identity;q=0").body(body).send().await.unwrap();
        assert_eq!(r.status(),200,"{typ}");
        assert_eq!(r.headers()["content-type"],"text/plain; charset=utf-8");
        assert_eq!(r.json::<serde_json::Value>().await.unwrap(),serde_json::json!([{"name":"target","path":path,"type":"file"}]));
        let r=client.get(format!("http://127.0.0.1:{port}/files")).query(&[("path",path.to_str().unwrap())]).send().await.unwrap();
        assert_eq!(r.text().await.unwrap(),"payload");
    }
    server.abort();
}

#[tokio::test]
async fn gzip_upload_errors_keep_the_actual_written_prefix() {
    use std::io::Write;
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("target");
    let (port, server) = support::spawn_daemon().await;
    let client = reqwest::Client::new();
    let payload = b"payload".repeat(1000);
    let mut encoder = flate2::write::GzEncoder::new(Vec::new(), flate2::Compression::default());
    encoder.write_all(&payload).unwrap();
    let mut corrupt = encoder.finish().unwrap();
    let end = corrupt.len();
    corrupt[end - 8] ^= 1;
    for (bytes, status, message, expected) in [
        (
            b"not gzip".to_vec(),
            400,
            "error decompressing request body: failed to create gzip reader: unexpected EOF",
            b"original".to_vec(),
        ),
        (
            vec![],
            400,
            "error decompressing request body: failed to create gzip reader: EOF",
            b"original".to_vec(),
        ),
        (
            corrupt,
            500,
            "error writing file: gzip: invalid checksum",
            payload,
        ),
    ] {
        std::fs::write(&path, b"original").unwrap();
        let r = client
            .post(format!("http://127.0.0.1:{port}/files"))
            .query(&[("path", path.to_str().unwrap())])
            .header("content-type", "application/octet-stream")
            .header("content-encoding", "gzip")
            .body(bytes)
            .send()
            .await
            .unwrap();
        assert_eq!(r.status(), status);
        assert_eq!(
            r.text().await.unwrap(),
            format!("{}\n", serde_json::json!({"code":status,"message":message}))
        );
        let r = client
            .get(format!("http://127.0.0.1:{port}/files"))
            .query(&[("path", path.to_str().unwrap())])
            .send()
            .await
            .unwrap();
        assert_eq!(r.bytes().await.unwrap().as_ref(), expected);
    }
    server.abort();
}

#[tokio::test]
async fn identity_seek_errors_follow_preconditions_and_gzip_can_stream() {
    let (port, server) = support::spawn_daemon().await;
    let client = reqwest::Client::new();
    let url = format!("http://127.0.0.1:{port}/files?path=/proc/version");
    let r = client.get(&url).send().await.unwrap();
    assert_eq!(r.status(), 500);
    assert_eq!(r.headers()["x-content-type-options"], "nosniff");
    assert!(!r.headers().contains_key("last-modified"));
    assert_eq!(r.text().await.unwrap(), "seeker can't seek\n");
    let r = client
        .get(&url)
        .header("if-none-match", "*")
        .send()
        .await
        .unwrap();
    assert_eq!(r.status(), 304);
    assert!(r.bytes().await.unwrap().is_empty());
    let r = client
        .get(&url)
        .header("accept-encoding", "gzip")
        .send()
        .await
        .unwrap();
    assert_eq!(r.status(), 200);
    assert_eq!(r.headers()["content-encoding"], "gzip");
    assert!(!r.bytes().await.unwrap().is_empty());
    server.abort();
}

#[tokio::test]
async fn gzip_framing_buffers_only_small_compressed_responses() {
    use std::io::Read;
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("unknown");
    let (port, server) = support::spawn_daemon().await;
    let mut random = vec![0; 100000];
    std::fs::File::open("/dev/urandom")
        .unwrap()
        .read_exact(&mut random)
        .unwrap();
    for (payload, small) in [(vec![0; 100000], true), (random, false)] {
        std::fs::write(&path, &payload).unwrap();
        let r = reqwest::Client::new()
            .get(format!("http://127.0.0.1:{port}/files"))
            .query(&[("path", path.to_str().unwrap())])
            .header("accept-encoding", "gzip")
            .send()
            .await
            .unwrap();
        assert_eq!(r.status(), 200);
        let length = r.content_length();
        assert_eq!(length.is_some(), small);
        let bytes = r.bytes().await.unwrap();
        if let Some(length) = length {
            assert_eq!(length, bytes.len() as u64);
        }
        let mut decoded = Vec::new();
        flate2::read::GzDecoder::new(bytes.as_ref())
            .read_to_end(&mut decoded)
            .unwrap();
        assert_eq!(decoded, payload);
    }
    server.abort();
}

#[tokio::test]
async fn upload_body_validation_precedes_format_and_raw_requires_a_path() {
    let (port, server) = support::spawn_daemon().await;
    let client = reqwest::Client::new();
    for (typ, coding, message) in [
        (
            "text/plain",
            "gzip",
            "error decompressing request body: failed to create gzip reader: unexpected EOF",
        ),
        (
            "application/octet-stream",
            "identity",
            "path query parameter is required for raw body upload",
        ),
    ] {
        let r = client
            .post(format!("http://127.0.0.1:{port}/files"))
            .header("content-type", typ)
            .header("content-encoding", coding)
            .body("not gzip")
            .send()
            .await
            .unwrap();
        assert_eq!(r.status(), 400);
        assert_eq!(
            r.json::<serde_json::Value>().await.unwrap(),
            serde_json::json!({"code":400,"message":message})
        );
    }
    let r = client
        .get(format!("http://127.0.0.1:{port}/files?path=/tmp/x%26y"))
        .send()
        .await
        .unwrap();
    assert_eq!(r.status(), 404);
    assert_eq!(
        r.text().await.unwrap(),
        "{\"code\":404,\"message\":\"path '/tmp/x\\u0026y' does not exist\"}\n"
    );
    server.abort();
}

#[tokio::test]
async fn download_loop_errors_use_the_file_lookup_status() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("loop");
    std::os::unix::fs::symlink("loop", &path).unwrap();
    let (port, server) = support::spawn_daemon().await;
    let r = reqwest::Client::new()
        .get(format!("http://127.0.0.1:{port}/files"))
        .query(&[("path", path.to_str().unwrap())])
        .send()
        .await
        .unwrap();
    assert_eq!(r.status(), 500);
    assert_eq!(
        r.json::<serde_json::Value>().await.unwrap(),
        serde_json::json!({"code":500,"message":format!("error checking if path exists '{}': stat {}: too many levels of symbolic links",path.display(),path.display())})
    );
    server.abort();
}

#[tokio::test]
async fn multipart_accepts_lf_delimiters_and_headers() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("lf");
    let (port, server) = support::spawn_daemon().await;
    let body = format!(
        "--B\nContent-Disposition: form-data; name=\"file\"; filename=\"{}\"\n\nfirst\n--B--\n",
        path.display()
    );
    let r = reqwest::Client::new()
        .post(format!("http://127.0.0.1:{port}/files"))
        .header("content-type", "multipart/form-data; boundary=B")
        .body(body)
        .send()
        .await
        .unwrap();
    assert_eq!(r.status(), 200);
    let r = reqwest::Client::new()
        .get(format!("http://127.0.0.1:{port}/files"))
        .query(&[("path", path.to_str().unwrap())])
        .send()
        .await
        .unwrap();
    assert_eq!(r.text().await.unwrap(), "first");
    server.abort();
}

#[tokio::test]
async fn multipart_duplicate_error_preserves_first_upload() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("duplicate");
    let (port, server) = support::spawn_daemon().await;
    let body=format!("--B\r\nContent-Disposition: form-data; name=file; filename=\"{}\"\r\n\r\nfirst\r\n--B\r\nContent-Disposition: form-data; name=file; filename=\"{}\"\r\n\r\nsecond\r\n--B--\r\n",path.display(),path.display());
    let client = reqwest::Client::new();
    let r = client
        .post(format!("http://127.0.0.1:{port}/files"))
        .header("content-type", "multipart/form-data; boundary=B")
        .body(body)
        .send()
        .await
        .unwrap();
    assert_eq!(r.status(), 400);
    assert_eq!(
        r.json::<serde_json::Value>().await.unwrap(),
        serde_json::json!({"code":400,"message":format!("you cannot upload multiple files to the same path '{}' in one upload request, only the first specified file was uploaded",path.display())})
    );
    let r = client
        .get(format!("http://127.0.0.1:{port}/files"))
        .query(&[("path", path.to_str().unwrap())])
        .send()
        .await
        .unwrap();
    assert_eq!(r.text().await.unwrap(), "first");
    server.abort();
}

#[tokio::test]
async fn multipart_extended_filename_overrides_plain_name() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("extended");
    let (port, server) = support::spawn_daemon().await;
    let encoded = path.to_str().unwrap().replace('/', "%2F");
    let body=format!("--B\r\nContent-Disposition: form-data; name=file; filename=ignored; filename*=UTF-8''{encoded}\r\n\r\nextended\r\n--B--\r\n");
    let client = reqwest::Client::new();
    let r = client
        .post(format!("http://127.0.0.1:{port}/files"))
        .header("content-type", "multipart/form-data; boundary=B")
        .body(body)
        .send()
        .await
        .unwrap();
    assert_eq!(r.status(), 200);
    let entries = r.json::<serde_json::Value>().await.unwrap();
    assert_eq!(entries[0]["path"], path.to_str().unwrap());
    let r = client
        .get(format!("http://127.0.0.1:{port}/files"))
        .query(&[("path", path.to_str().unwrap())])
        .send()
        .await
        .unwrap();
    assert_eq!(r.text().await.unwrap(), "extended");
    server.abort();
}

#[tokio::test]
async fn multipart_decodes_quoted_printable_before_writing() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("quoted-printable");
    let (port, server) = support::spawn_daemon().await;
    let client = reqwest::Client::new();
    for (encoded, expected) in [
        ("a=3Db", "a=b"),
        ("a=\r\nb=0aend=\n", "ab\nend"),
        ("a  \r\nb=3d=xy", "a\r\nb==xy"),
    ] {
        let body=format!("--B\r\nContent-Disposition: form-data; name=file; filename=\"{}\"\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n{encoded}\r\n--B--\r\n",path.display());
        let r = client
            .post(format!("http://127.0.0.1:{port}/files"))
            .header("content-type", "multipart/form-data; boundary=B")
            .body(body)
            .send()
            .await
            .unwrap();
        assert_eq!(r.status(), 200);
        let r = client
            .get(format!("http://127.0.0.1:{port}/files"))
            .query(&[("path", path.to_str().unwrap())])
            .send()
            .await
            .unwrap();
        assert_eq!(r.text().await.unwrap(), expected);
    }
    server.abort();
}

#[tokio::test]
async fn multipart_continuations_join_utf8_bytes() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("中.txt");
    let (port, server) = support::spawn_daemon().await;
    let prefix = dir.path().to_str().unwrap().replace('/', "%2F");
    let body=format!("--B\r\nContent-Disposition: form-data; name=file; filename*0*=UTF-8''{prefix}%2F%E4; filename*1*=%B8%AD.txt\r\n\r\ncontinued\r\n--B--\r\n");
    let client = reqwest::Client::new();
    let r = client
        .post(format!("http://127.0.0.1:{port}/files"))
        .header("content-type", "multipart/form-data; boundary=B")
        .body(body)
        .send()
        .await
        .unwrap();
    assert_eq!(r.status(), 200);
    assert_eq!(
        r.json::<serde_json::Value>().await.unwrap()[0]["path"],
        path.to_str().unwrap()
    );
    let r = client
        .get(format!("http://127.0.0.1:{port}/files"))
        .query(&[("path", path.to_str().unwrap())])
        .send()
        .await
        .unwrap();
    assert_eq!(r.text().await.unwrap(), "continued");
    server.abort();
}

#[tokio::test]
async fn null_device_download_matches_upstream() {
    let (port, server) = support::spawn_daemon().await;
    let client = reqwest::Client::new();
    for encoding in ["identity", "gzip"] {
        let response = client
            .get(format!("http://127.0.0.1:{port}/files?path=/dev/null"))
            .header("accept-encoding", encoding)
            .send()
            .await
            .unwrap();
        assert_eq!(response.status(), 200, "{encoding}");
        let body = response.bytes().await.unwrap();
        if encoding == "identity" {
            assert!(body.is_empty());
        } else {
            let mut decoded = Vec::new();
            std::io::Read::read_to_end(
                &mut flate2::read::GzDecoder::new(body.as_ref()),
                &mut decoded,
            )
            .unwrap();
            assert!(decoded.is_empty());
        }
    }
    server.abort();
}

#[tokio::test]
async fn null_device_upload_uses_upstream_device_semantics() {
    let (port, server) = support::spawn_daemon().await;
    let client = reqwest::Client::new();
    let response = client
        .post(format!("http://127.0.0.1:{port}/files?path=/dev/null"))
        .header("content-type", "application/octet-stream")
        .body("discarded")
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 200);
    let response = client
        .get(format!("http://127.0.0.1:{port}/files?path=/dev/null"))
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 200);
    assert!(response.bytes().await.unwrap().is_empty());
    server.abort();
}
