use std::env;
use std::fs;
use std::path::{Path, PathBuf};

// 列出需要转换为 Rust 类型的 protobuf 源文件。
const PROTOS: &[&str] = &[
    "proto/process/process.proto",
    "proto/filesystem/filesystem.proto",
];
// 列出生成后需要同步到源码树的 Rust 文件名。
const GENERATED: &[&str] = &[
    "process.rs",
    "filesystem.rs",
    "process.serde.rs",
    "filesystem.serde.rs",
];

// 仅在 protobuf 生成结果变化时覆盖目标文件，避免无意义的工作区变更。
fn write_if_changed(source: &Path, destination: &Path) {
    let generated = fs::read(source).expect("read generated protobuf source");
    let existing = fs::read(destination).ok();

    if existing.as_deref() != Some(generated.as_slice()) {
        fs::write(destination, generated).expect("write generated protobuf source");
    }
}

// 配置 vendored protoc 并生成、同步 protobuf 对应的 Rust 类型。
fn main() {
    for path in PROTOS {
        println!("cargo:rerun-if-changed={path}");
    }
    println!("cargo:rerun-if-changed=proto/google/protobuf/timestamp.proto");
    println!("cargo:rerun-if-changed=build.rs");

    let protoc = protoc_bin_vendored::protoc_bin_path().expect("find vendored protoc");
    unsafe {
        env::set_var("PROTOC", protoc);
    }

    let out_dir = PathBuf::from(env::var("OUT_DIR").expect("Cargo OUT_DIR"));
    let temporary_output = out_dir.join("protobuf");
    let descriptor = out_dir.join("cube-envd-proto-descriptor.bin");
    fs::create_dir_all(&temporary_output).expect("create protobuf output directory");

    let mut config = prost_build::Config::new();
    config.out_dir(&temporary_output);
    config.compile_well_known_types();
    config.file_descriptor_set_path(&descriptor);
    config.extern_path(".google.protobuf", "::pbjson_types");
    config
        .compile_protos(PROTOS, &["proto"])
        .expect("compile vendored protobuf definitions");

    let descriptors = fs::read(&descriptor).expect("read protobuf descriptor set");
    pbjson_build::Builder::new()
        .register_descriptors(&descriptors)
        .expect("register protobuf descriptors")
        .build(&[".process", ".filesystem"])
        .expect("generate protobuf JSON mappings");

    let destination = Path::new("src/generated");
    fs::create_dir_all(destination).expect("create generated source directory");
    for name in GENERATED {
        let source = if name.ends_with(".serde.rs") {
            out_dir.join(name)
        } else {
            temporary_output.join(name)
        };
        write_if_changed(&source, &destination.join(name));
    }
}
