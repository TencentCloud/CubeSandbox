// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

//! Translate decoder diagnostics without changing decoding or accepting a partial message.
use serde::de::{MapAccess, Visitor};
use serde::Deserialize;
use serde_json::value::RawValue;

pub(super) struct Fields<'a>(pub(super) Vec<(String, &'a RawValue)>);
impl<'de> Deserialize<'de> for Fields<'de> {
    fn deserialize<D: serde::Deserializer<'de>>(decoder: D) -> Result<Self, D::Error> {
        struct Object;
        impl<'de> Visitor<'de> for Object {
            type Value = Fields<'de>;
            fn expecting(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                f.write_str("object")
            }
            fn visit_map<A: MapAccess<'de>>(self, mut map: A) -> Result<Self::Value, A::Error> {
                let mut fields = Vec::new();
                while let Some(field) = map.next_entry()? {
                    fields.push(field);
                }
                Ok(Fields(fields))
            }
        }
        decoder.deserialize_map(Object)
    }
}
struct Field<'a> {
    key: String,
    key_offset: usize,
    value_offset: usize,
    value: &'a str,
    duplicate: bool,
}
fn field_at(input: &[u8], error_offset: usize) -> Option<Field<'_>> {
    let root = serde_json::from_slice::<&RawValue>(input).ok()?;
    let base = input.as_ptr() as usize;
    let mut pending = vec![root];
    let mut found: Option<Field<'_>> = None;
    while let Some(object) = pending.pop() {
        let Ok(Fields(fields)) = serde_json::from_str::<Fields<'_>>(object.get()) else {
            continue;
        };
        let mut previous_end = object.get().as_ptr() as usize - base + 1;
        let mut names = std::collections::HashSet::new();
        for (key, value) in fields {
            let value_offset = value.get().as_ptr() as usize - base;
            let key_offset = previous_end
                + input[previous_end..value_offset]
                    .iter()
                    .position(|byte| *byte == b'"')?;
            let duplicate = !names.insert(key.clone());
            previous_end = value_offset + value.get().len();
            if value.get().starts_with('{') {
                pending.push(value);
            }
            if key_offset <= error_offset
                && found
                    .as_ref()
                    .is_none_or(|field| field.key_offset < key_offset)
            {
                found = Some(Field {
                    key,
                    key_offset,
                    value_offset,
                    value: value.get(),
                    duplicate,
                });
            }
        }
    }
    found
}
fn position(input: &[u8], offset: usize) -> String {
    let prefix = &input[..offset.min(input.len())];
    let line = prefix.iter().filter(|byte| **byte == b'\n').count() + 1;
    let column = prefix.len()
        - prefix
            .iter()
            .rposition(|byte| *byte == b'\n')
            .map_or(0, |index| index + 1)
        + 1;
    format!("line {line}:{column}")
}
fn offset(input: &[u8], line: usize, column: usize) -> usize {
    let start = if line <= 1 {
        0
    } else {
        input
            .iter()
            .enumerate()
            .filter(|(_, byte)| **byte == b'\n')
            .nth(line - 2)
            .map_or(input.len(), |(index, _)| index + 1)
    };
    (start + column.saturating_sub(1)).min(input.len())
}

pub(super) fn message(name: &str, input: &[u8], error: &serde_json::Error) -> String {
    if input.is_empty() {
        return "unmarshal message: zero-length payload is not a valid JSON object".into();
    }
    let prefix = format!("unmarshal message: unmarshal into *{name}: proto: ");
    if error.is_eof() {
        return format!("{prefix}unexpected EOF");
    }
    let diagnostic = error.to_string();
    let at = offset(input, error.line(), error.column());
    let field = field_at(input, at);
    if diagnostic.contains("multiple oneof fields set") || diagnostic.starts_with("duplicate field")
    {
        if let Some(field) = field.as_ref() {
            let where_ = position(input, field.key_offset);
            if field.duplicate || diagnostic.starts_with("duplicate field") {
                return format!("{prefix}({where_}): duplicate field {:?}", field.key);
            }
            let oneof = diagnostic.split('\'').nth(1).unwrap_or("");
            let message = match oneof {
                "selector" => "process.ProcessSelector",
                "input" => "process.ProcessInput",
                "event" => "process.StreamInputRequest",
                _ => name,
            };
            return format!(
                "{prefix}({where_}): error parsing {:?}, oneof {message}.{oneof} is already set",
                field.key
            );
        }
    }
    if let Some(field) = field.as_ref() {
        let scalar = if diagnostic.contains("expected a u32") {
            Some("uint32")
        } else if diagnostic.contains("expected an i32") {
            Some("int32")
        } else if diagnostic.contains("base64") {
            Some("bytes")
        } else if diagnostic.contains("expected a string") {
            Some("string")
        } else if diagnostic.contains("expected a boolean") {
            Some("bool")
        } else {
            None
        };
        if let Some(scalar) = scalar {
            return format!(
                "{prefix}({}): invalid value for {scalar} field {}: {}",
                position(input, field.value_offset),
                field.key,
                field.value
            );
        }
    }
    if diagnostic.contains("expected struct") || diagnostic.starts_with("trailing characters") {
        let start = if diagnostic.starts_with("trailing characters") {
            at
        } else if let Some(field) = field {
            field.value_offset
        } else {
            input
                .iter()
                .position(|byte| !byte.is_ascii_whitespace())
                .unwrap_or(0)
        };
        let tail = &input[start..];
        let token = if matches!(tail.first(), Some(b'[' | b'{')) {
            String::from_utf8_lossy(&tail[..1]).into_owned()
        } else {
            serde_json::Deserializer::from_slice(tail)
                .into_iter::<&RawValue>()
                .next()
                .and_then(Result::ok)
                .map(|value| value.get().to_owned())
                .unwrap_or_else(|| String::from_utf8_lossy(&tail[..tail.len().min(1)]).into_owned())
        };
        return format!(
            "{prefix}syntax error ({}): unexpected token {token}",
            position(input, start)
        );
    }
    format!("{prefix}{diagnostic}")
}
