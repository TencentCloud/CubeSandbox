// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

//! ProtoJSON messages must be objects. Serde's derived struct decoder also
//! accepts positional arrays; redirect only that entry point to its map decoder.
//! Parsing still uses serde_json over a complete, decompressed RPC message.
use connectrpc::interceptor::{StreamRequest, StreamResponse, UnaryRequest, UnaryResponse};
use connectrpc::{
    CodecFormat, ConnectError, ErrorCode, Interceptor, Next, NextStream, Payload, PayloadStream,
};
use futures::StreamExt;
use serde::de::{
    self, DeserializeSeed, Deserializer, EnumAccess, MapAccess, SeqAccess, VariantAccess, Visitor,
};

struct Objects<D>(D);
struct ObjectVisitor<V>(V);
struct Seed<S>(S);
struct Map<M>(M);
struct Seq<S>(S);
struct Enum<E>(E);
struct Variant<V>(V);

impl<'de, S: DeserializeSeed<'de>> DeserializeSeed<'de> for Seed<S> {
    type Value = S::Value;
    fn deserialize<D: Deserializer<'de>>(self, d: D) -> Result<Self::Value, D::Error> {
        self.0.deserialize(Objects(d))
    }
}

impl<'de, D: Deserializer<'de>> Deserializer<'de> for Objects<D> {
    type Error = D::Error;
    fn deserialize_any<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, Self::Error> {
        self.0.deserialize_any(ObjectVisitor(visitor))
    }
    fn deserialize_bool<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, Self::Error> {
        self.0.deserialize_bool(ObjectVisitor(visitor))
    }
    fn deserialize_i8<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, Self::Error> {
        self.0.deserialize_i8(ObjectVisitor(visitor))
    }
    fn deserialize_i16<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, Self::Error> {
        self.0.deserialize_i16(ObjectVisitor(visitor))
    }
    fn deserialize_i32<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, Self::Error> {
        self.0.deserialize_i32(ObjectVisitor(visitor))
    }
    fn deserialize_i64<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, Self::Error> {
        self.0.deserialize_i64(ObjectVisitor(visitor))
    }
    fn deserialize_i128<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, Self::Error> {
        self.0.deserialize_i128(ObjectVisitor(visitor))
    }
    fn deserialize_u8<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, Self::Error> {
        self.0.deserialize_u8(ObjectVisitor(visitor))
    }
    fn deserialize_u16<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, Self::Error> {
        self.0.deserialize_u16(ObjectVisitor(visitor))
    }
    fn deserialize_u32<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, Self::Error> {
        self.0.deserialize_u32(ObjectVisitor(visitor))
    }
    fn deserialize_u64<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, Self::Error> {
        self.0.deserialize_u64(ObjectVisitor(visitor))
    }
    fn deserialize_u128<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, Self::Error> {
        self.0.deserialize_u128(ObjectVisitor(visitor))
    }
    fn deserialize_f32<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, Self::Error> {
        self.0.deserialize_f32(ObjectVisitor(visitor))
    }
    fn deserialize_f64<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, Self::Error> {
        self.0.deserialize_f64(ObjectVisitor(visitor))
    }
    fn deserialize_char<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, Self::Error> {
        self.0.deserialize_char(ObjectVisitor(visitor))
    }
    fn deserialize_str<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, Self::Error> {
        self.0.deserialize_str(ObjectVisitor(visitor))
    }
    fn deserialize_string<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, Self::Error> {
        self.0.deserialize_string(ObjectVisitor(visitor))
    }
    fn deserialize_bytes<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, Self::Error> {
        self.0.deserialize_bytes(ObjectVisitor(visitor))
    }
    fn deserialize_byte_buf<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, Self::Error> {
        self.0.deserialize_byte_buf(ObjectVisitor(visitor))
    }
    fn deserialize_option<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, Self::Error> {
        self.0.deserialize_option(ObjectVisitor(visitor))
    }
    fn deserialize_unit<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, Self::Error> {
        self.0.deserialize_unit(ObjectVisitor(visitor))
    }
    fn deserialize_unit_struct<V: Visitor<'de>>(
        self,
        name: &'static str,
        visitor: V,
    ) -> Result<V::Value, Self::Error> {
        self.0.deserialize_unit_struct(name, ObjectVisitor(visitor))
    }
    fn deserialize_newtype_struct<V: Visitor<'de>>(
        self,
        name: &'static str,
        visitor: V,
    ) -> Result<V::Value, Self::Error> {
        self.0
            .deserialize_newtype_struct(name, ObjectVisitor(visitor))
    }
    fn deserialize_seq<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, Self::Error> {
        self.0.deserialize_seq(ObjectVisitor(visitor))
    }
    fn deserialize_tuple<V: Visitor<'de>>(
        self,
        len: usize,
        visitor: V,
    ) -> Result<V::Value, Self::Error> {
        self.0.deserialize_tuple(len, ObjectVisitor(visitor))
    }
    fn deserialize_tuple_struct<V: Visitor<'de>>(
        self,
        name: &'static str,
        len: usize,
        visitor: V,
    ) -> Result<V::Value, Self::Error> {
        self.0
            .deserialize_tuple_struct(name, len, ObjectVisitor(visitor))
    }
    fn deserialize_map<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, Self::Error> {
        self.0.deserialize_map(ObjectVisitor(visitor))
    }
    fn deserialize_enum<V: Visitor<'de>>(
        self,
        name: &'static str,
        variants: &'static [&'static str],
        visitor: V,
    ) -> Result<V::Value, Self::Error> {
        self.0
            .deserialize_enum(name, variants, ObjectVisitor(visitor))
    }
    fn deserialize_identifier<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, Self::Error> {
        self.0.deserialize_identifier(ObjectVisitor(visitor))
    }
    fn deserialize_ignored_any<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, Self::Error> {
        self.0.deserialize_ignored_any(ObjectVisitor(visitor))
    }
    fn deserialize_struct<V: Visitor<'de>>(
        self,
        _name: &'static str,
        _fields: &'static [&'static str],
        visitor: V,
    ) -> Result<V::Value, Self::Error> {
        self.0.deserialize_map(ObjectVisitor(visitor))
    }
    fn is_human_readable(&self) -> bool {
        self.0.is_human_readable()
    }
}

impl<'de, V: Visitor<'de>> Visitor<'de> for ObjectVisitor<V> {
    type Value = V::Value;
    fn expecting(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        self.0.expecting(f)
    }
    fn visit_bool<E: de::Error>(self, value: bool) -> Result<V::Value, E> {
        self.0.visit_bool(value)
    }
    fn visit_i8<E: de::Error>(self, value: i8) -> Result<V::Value, E> {
        self.0.visit_i8(value)
    }
    fn visit_i16<E: de::Error>(self, value: i16) -> Result<V::Value, E> {
        self.0.visit_i16(value)
    }
    fn visit_i32<E: de::Error>(self, value: i32) -> Result<V::Value, E> {
        self.0.visit_i32(value)
    }
    fn visit_i64<E: de::Error>(self, value: i64) -> Result<V::Value, E> {
        self.0.visit_i64(value)
    }
    fn visit_i128<E: de::Error>(self, value: i128) -> Result<V::Value, E> {
        self.0.visit_i128(value)
    }
    fn visit_u8<E: de::Error>(self, value: u8) -> Result<V::Value, E> {
        self.0.visit_u8(value)
    }
    fn visit_u16<E: de::Error>(self, value: u16) -> Result<V::Value, E> {
        self.0.visit_u16(value)
    }
    fn visit_u32<E: de::Error>(self, value: u32) -> Result<V::Value, E> {
        self.0.visit_u32(value)
    }
    fn visit_u64<E: de::Error>(self, value: u64) -> Result<V::Value, E> {
        self.0.visit_u64(value)
    }
    fn visit_u128<E: de::Error>(self, value: u128) -> Result<V::Value, E> {
        self.0.visit_u128(value)
    }
    fn visit_f32<E: de::Error>(self, value: f32) -> Result<V::Value, E> {
        self.0.visit_f32(value)
    }
    fn visit_f64<E: de::Error>(self, value: f64) -> Result<V::Value, E> {
        self.0.visit_f64(value)
    }
    fn visit_char<E: de::Error>(self, value: char) -> Result<V::Value, E> {
        self.0.visit_char(value)
    }
    fn visit_str<E: de::Error>(self, value: &str) -> Result<V::Value, E> {
        self.0.visit_str(value)
    }
    fn visit_borrowed_str<E: de::Error>(self, value: &'de str) -> Result<V::Value, E> {
        self.0.visit_borrowed_str(value)
    }
    fn visit_string<E: de::Error>(self, value: String) -> Result<V::Value, E> {
        self.0.visit_string(value)
    }
    fn visit_bytes<E: de::Error>(self, value: &[u8]) -> Result<V::Value, E> {
        self.0.visit_bytes(value)
    }
    fn visit_borrowed_bytes<E: de::Error>(self, value: &'de [u8]) -> Result<V::Value, E> {
        self.0.visit_borrowed_bytes(value)
    }
    fn visit_byte_buf<E: de::Error>(self, value: Vec<u8>) -> Result<V::Value, E> {
        self.0.visit_byte_buf(value)
    }
    fn visit_none<E: de::Error>(self) -> Result<V::Value, E> {
        self.0.visit_none()
    }
    fn visit_unit<E: de::Error>(self) -> Result<V::Value, E> {
        self.0.visit_unit()
    }
    fn visit_some<D: Deserializer<'de>>(self, d: D) -> Result<V::Value, D::Error> {
        self.0.visit_some(Objects(d))
    }
    fn visit_newtype_struct<D: Deserializer<'de>>(self, d: D) -> Result<V::Value, D::Error> {
        self.0.visit_newtype_struct(Objects(d))
    }
    fn visit_map<M: MapAccess<'de>>(self, m: M) -> Result<V::Value, M::Error> {
        self.0.visit_map(Map(m))
    }
    fn visit_seq<S: SeqAccess<'de>>(self, s: S) -> Result<V::Value, S::Error> {
        self.0.visit_seq(Seq(s))
    }
    fn visit_enum<E: EnumAccess<'de>>(self, e: E) -> Result<V::Value, E::Error> {
        self.0.visit_enum(Enum(e))
    }
}
impl<'de, M: MapAccess<'de>> MapAccess<'de> for Map<M> {
    type Error = M::Error;
    fn next_key_seed<K: DeserializeSeed<'de>>(
        &mut self,
        seed: K,
    ) -> Result<Option<K::Value>, Self::Error> {
        self.0.next_key_seed(Seed(seed))
    }
    fn next_value_seed<V: DeserializeSeed<'de>>(
        &mut self,
        seed: V,
    ) -> Result<V::Value, Self::Error> {
        self.0.next_value_seed(Seed(seed))
    }
    fn size_hint(&self) -> Option<usize> {
        self.0.size_hint()
    }
}
impl<'de, S: SeqAccess<'de>> SeqAccess<'de> for Seq<S> {
    type Error = S::Error;
    fn next_element_seed<V: DeserializeSeed<'de>>(
        &mut self,
        seed: V,
    ) -> Result<Option<V::Value>, Self::Error> {
        self.0.next_element_seed(Seed(seed))
    }
    fn size_hint(&self) -> Option<usize> {
        self.0.size_hint()
    }
}
impl<'de, E: EnumAccess<'de>> EnumAccess<'de> for Enum<E> {
    type Error = E::Error;
    type Variant = Variant<E::Variant>;
    fn variant_seed<V: DeserializeSeed<'de>>(
        self,
        seed: V,
    ) -> Result<(V::Value, Self::Variant), Self::Error> {
        self.0
            .variant_seed(Seed(seed))
            .map(|(v, a)| (v, Variant(a)))
    }
}
impl<'de, V: VariantAccess<'de>> VariantAccess<'de> for Variant<V> {
    type Error = V::Error;
    fn unit_variant(self) -> Result<(), Self::Error> {
        self.0.unit_variant()
    }
    fn newtype_variant_seed<T: DeserializeSeed<'de>>(
        self,
        seed: T,
    ) -> Result<T::Value, Self::Error> {
        self.0.newtype_variant_seed(Seed(seed))
    }
    fn tuple_variant<T: Visitor<'de>>(
        self,
        len: usize,
        visitor: T,
    ) -> Result<T::Value, Self::Error> {
        self.0.tuple_variant(len, ObjectVisitor(visitor))
    }
    fn struct_variant<T: Visitor<'de>>(
        self,
        fields: &'static [&'static str],
        visitor: T,
    ) -> Result<T::Value, Self::Error> {
        self.0.struct_variant(fields, ObjectVisitor(visitor))
    }
}

fn decode<T: connectrpc::AnyMessage + buffa::Message + serde::Serialize + de::DeserializeOwned>(
    payload: Payload,
    name: &str,
) -> Result<Payload, ConnectError> {
    if payload.format() == CodecFormat::Proto {
        payload.message::<T>().map_err(|_|ConnectError::invalid_argument(format!("unmarshal message: unmarshal into *{name}: proto: cannot parse invalid wire-format data")))?;
        return Ok(payload);
    }
    let mut decoder = serde_json::Deserializer::from_slice(payload.bytes());
    let message = T::deserialize(Objects(&mut decoder))
        .and_then(|message| {
            decoder.end()?;
            Ok(message)
        })
        .map_err(|error| {
            ConnectError::new(
                ErrorCode::InvalidArgument,
                super::json_error::message(name, payload.bytes(), &error),
            )
        })?;
    Ok(Payload::from_message(message, CodecFormat::Json))
}

pub(super) fn message(path: &str, payload: Payload) -> Result<Payload, ConnectError> {
    let method = path.rsplit('/').next().unwrap_or("");
    let namespace = path.trim_start_matches('/').split('.').next().unwrap_or("");
    let name = format!("{namespace}.{method}Request");
    match path {
        "process.Process/List" | "/process.Process/List" => {
            decode::<crate::proto::process::ListRequest>(payload, &name)
        }
        "process.Process/Connect" | "/process.Process/Connect" => {
            decode::<crate::proto::process::ConnectRequest>(payload, &name)
        }
        "process.Process/Start" | "/process.Process/Start" => {
            decode::<crate::proto::process::StartRequest>(payload, &name)
        }
        "process.Process/Update" | "/process.Process/Update" => {
            decode::<crate::proto::process::UpdateRequest>(payload, &name)
        }
        "process.Process/StreamInput" | "/process.Process/StreamInput" => {
            decode::<crate::proto::process::StreamInputRequest>(payload, &name)
        }
        "process.Process/SendInput" | "/process.Process/SendInput" => {
            decode::<crate::proto::process::SendInputRequest>(payload, &name)
        }
        "process.Process/SendSignal" | "/process.Process/SendSignal" => {
            decode::<crate::proto::process::SendSignalRequest>(payload, &name)
        }
        "process.Process/CloseStdin" | "/process.Process/CloseStdin" => {
            decode::<crate::proto::process::CloseStdinRequest>(payload, &name)
        }
        "filesystem.Filesystem/Stat" | "/filesystem.Filesystem/Stat" => {
            decode::<crate::proto::filesystem::StatRequest>(payload, &name)
        }
        "filesystem.Filesystem/MakeDir" | "/filesystem.Filesystem/MakeDir" => {
            decode::<crate::proto::filesystem::MakeDirRequest>(payload, &name)
        }
        "filesystem.Filesystem/Move" | "/filesystem.Filesystem/Move" => {
            decode::<crate::proto::filesystem::MoveRequest>(payload, &name)
        }
        "filesystem.Filesystem/ListDir" | "/filesystem.Filesystem/ListDir" => {
            decode::<crate::proto::filesystem::ListDirRequest>(payload, &name)
        }
        "filesystem.Filesystem/Remove" | "/filesystem.Filesystem/Remove" => {
            decode::<crate::proto::filesystem::RemoveRequest>(payload, &name)
        }
        "filesystem.Filesystem/WatchDir" | "/filesystem.Filesystem/WatchDir" => {
            decode::<crate::proto::filesystem::WatchDirRequest>(payload, &name)
        }
        "filesystem.Filesystem/CreateWatcher" | "/filesystem.Filesystem/CreateWatcher" => {
            decode::<crate::proto::filesystem::CreateWatcherRequest>(payload, &name)
        }
        "filesystem.Filesystem/GetWatcherEvents" | "/filesystem.Filesystem/GetWatcherEvents" => {
            decode::<crate::proto::filesystem::GetWatcherEventsRequest>(payload, &name)
        }
        "filesystem.Filesystem/RemoveWatcher" | "/filesystem.Filesystem/RemoveWatcher" => {
            decode::<crate::proto::filesystem::RemoveWatcherRequest>(payload, &name)
        }
        _ => Ok(payload),
    }
}

pub(super) struct ProtoJson;
#[connectrpc::async_trait]
impl Interceptor for ProtoJson {
    async fn intercept_unary(
        &self,
        mut request: UnaryRequest,
        next: Next<'_>,
    ) -> Result<UnaryResponse, ConnectError> {
        request.payload = message(request.ctx.path().unwrap_or(""), request.payload)?;
        next.run(request).await
    }
    async fn intercept_streaming(
        &self,
        request: StreamRequest,
        inbound: PayloadStream,
        next: NextStream<'_>,
    ) -> Result<StreamResponse, ConnectError> {
        let path = request.ctx.path().unwrap_or("").to_owned();
        let frame_error = request
            .ctx
            .extensions()
            .get::<super::framing::FrameError>()
            .cloned();
        next.run(
            request,
            Box::pin(inbound.map(move |payload| {
                payload
                    .map_err(|error| {
                        frame_error
                            .as_ref()
                            .and_then(|state| state.get())
                            .unwrap_or(error)
                    })
                    .and_then(|payload| message(&path, payload))
            })),
        )
        .await
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn protocol_objects_preserve_wire_types_and_reject_positional_arrays() {
        for (path, valid, invalid) in [
            (
                "/process.Process/Start",
                r#"{"process":{"cmd":"sh","args":["-c","true"]}}"#,
                r#"{"process":[]}"#,
            ),
            (
                "/filesystem.Filesystem/Stat",
                r#"{"path":"/tmp"}"#,
                r#"{"path":2}"#,
            ),
        ] {
            let payload = |body: &str| {
                Payload::new(
                    bytes::Bytes::copy_from_slice(body.as_bytes()),
                    CodecFormat::Json,
                )
            };
            assert!(message(path, payload(valid)).is_ok());
            assert!(message(path, payload(invalid)).is_err());
            assert!(message(path, payload("[]")).is_err());
        }
    }
}
