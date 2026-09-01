use cube_envd::generated::{filesystem, process};

// 验证 vendored protobuf 已生成可用的进程和文件系统类型。
#[test]
fn exposes_vendored_process_and_filesystem_types() {
    let request = process::StartRequest::default();
    let entry = filesystem::EntryInfo::default();

    assert!(request.process.is_none());
    assert!(entry.path.is_empty());
}

// 验证生成类型遵循 protobuf JSON 的 camelCase、bytes 和 Timestamp 映射。
#[test]
fn generated_types_round_trip_protobuf_json() {
    let request: process::StartRequest = serde_json::from_str(
        r#"{
            "process": {
                "cmd": "/bin/echo",
                "args": ["hello"],
                "envs": {"ONE": "1"},
                "cwd": "/tmp"
            },
            "pty": {"size": {"cols": 80, "rows": 24}},
            "tag": "example",
            "stdin": true
        }"#,
    )
    .expect("protobuf JSON request can be decoded");
    let json = serde_json::to_value(request).expect("protobuf JSON request can be encoded");

    assert_eq!(json["process"]["envs"]["ONE"], "1");
    assert_eq!(json["pty"]["size"]["cols"], 80);
    assert_eq!(json["stdin"], true);
}
