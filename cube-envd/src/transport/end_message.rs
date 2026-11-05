// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use super::json_error::Fields;
use connectrpc::ConnectError;
use serde_json::value::RawValue;

fn kind(raw: &RawValue) -> &'static str {
    match raw.get().as_bytes()[0] {
        b'{' => "object",
        b'[' => "array",
        b'"' => "string",
        b't' | b'f' => "bool",
        _ => "number",
    }
}
fn mismatch(raw: &RawValue, path: &str, expected: &str) -> ConnectError {
    let target = if path.is_empty() {
        "Go value".to_owned()
    } else {
        format!("Go struct field connectEndStreamMessage.{path}")
    };
    ConnectError::internal(format!(
        "unmarshal end stream message: json: cannot unmarshal {} into {target} of type {expected}",
        kind(raw)
    ))
}
fn fields<'a>(raw: &'a RawValue, path: &str, expected: &str) -> Result<Fields<'a>, ConnectError> {
    if raw.get() == "null" {
        return Ok(Fields(Vec::new()));
    }
    serde_json::from_str(raw.get()).map_err(|_| mismatch(raw, path, expected))
}
fn string(raw: &RawValue, path: &str) -> Result<(), ConnectError> {
    if raw.get() == "null" || raw.get().starts_with('"') {
        Ok(())
    } else {
        Err(mismatch(raw, path, "string"))
    }
}
fn array<'a>(
    raw: &'a RawValue,
    path: &str,
    expected: &str,
) -> Result<Vec<&'a RawValue>, ConnectError> {
    if raw.get() == "null" {
        return Ok(Vec::new());
    }
    serde_json::from_str(raw.get()).map_err(|_| mismatch(raw, path, expected))
}

pub(super) fn validate(bytes: &[u8]) -> Result<(), ConnectError> {
    let raw: &RawValue = serde_json::from_slice(bytes).map_err(|error| {
        let detail = if error.is_eof() {
            "unexpected end of JSON input".to_owned()
        } else {
            error.to_string()
        };
        ConnectError::internal(format!("unmarshal end stream message: {detail}"))
    })?;
    for (key, value) in fields(raw, "", "connect.connectEndStreamMessage")?.0 {
        if key.eq_ignore_ascii_case("metadata") {
            for (_, values) in fields(value, "metadata", "http.Header")?.0 {
                for value in array(values, "metadata", "[]string")? {
                    string(value, "metadata")?;
                }
            }
        } else if key.eq_ignore_ascii_case("error") {
            for (key,value) in fields(value,"error",r#"struct { Code string "json:\"code\""; Message string "json:\"message\""; Details []*connect.connectWireDetail "json:\"details\"" }"#)?.0 {
                if key.eq_ignore_ascii_case("code") { string(value,"error.code")?; }
                else if key.eq_ignore_ascii_case("message") { string(value,"error.message")?; }
                else if key.eq_ignore_ascii_case("details") {
                    for detail in array(value,"error.details","[]*connect.connectWireDetail")? {
                        for (key,value) in fields(detail,"error.details",r#"struct { Type string "json:\"type\""; Value string "json:\"value\"" }"#)?.0 {
                            if key.eq_ignore_ascii_case("type") { string(value,"error.details.type")?; }
                            else if key.eq_ignore_ascii_case("value") {
                                string(value,"error.details.value")?;
                                if value.get() != "null" {
                                    use base64::Engine;
                                    let text: String = serde_json::from_str(value.get()).expect("checked JSON string");
                                    let text: String = text.chars().filter(|character|!matches!(character,'\r'|'\n')).collect();
                                    if let Err(error) = base64::engine::general_purpose::STANDARD.decode(&text).or_else(|_|base64::engine::general_purpose::STANDARD_NO_PAD.decode(&text)) {
                                        let at = match error { base64::DecodeError::InvalidByte(index,_) | base64::DecodeError::InvalidLastSymbol(index,_) => index, base64::DecodeError::InvalidLength(length) => length.saturating_sub(1), base64::DecodeError::InvalidPadding => text.len().saturating_sub(1) };
                                        return Err(ConnectError::internal(format!("unmarshal end stream message: decode base64: illegal base64 data at input byte {at}")));
                                    }
                                }
                            }
                        }
                    }
                }
            }
        }
    }
    Ok(())
}
