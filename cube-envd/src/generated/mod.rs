/// 提供 Filesystem protobuf 类型及其 JSON 映射。
#[allow(clippy::large_enum_variant)]
#[rustfmt::skip]
pub mod filesystem {
    include!("filesystem.rs");
    include!("filesystem.serde.rs");
}

/// 提供 Process protobuf 类型及其 JSON 映射。
#[rustfmt::skip]
pub mod process {
    include!("process.rs");
    include!("process.serde.rs");
}
