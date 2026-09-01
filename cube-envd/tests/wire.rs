use cube_envd::{generated::process, wire};

// 验证 wire 层以 protobuf JSON 规则处理生成的请求和响应类型。
#[test]
fn protobuf_json_round_trip_uses_generated_types() {
    let request: process::SendInputRequest = wire::decode_json(
        br#"{
            "process": {"tag": "task"},
            "input": {"stdin": "aGVsbG8="}
        }"#,
        "SendInput request",
    )
    .expect("protobuf JSON request is decoded");

    let json = wire::encode_json(&request).expect("protobuf JSON request is encoded");
    let json: serde_json::Value =
        serde_json::from_slice(&json).expect("encoded protobuf JSON is valid JSON");

    assert_eq!(json["process"]["tag"], "task");
    assert_eq!(json["input"]["stdin"], "aGVsbG8=");
}
