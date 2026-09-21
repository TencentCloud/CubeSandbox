// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! 层规则的源码断言：`cargo test` 就能挡住反向依赖。
//!
//! 为什么必须读源码：模块路径不在类型系统里。`pub(crate)` 对同级模块一律可见，
//! 一个指错方向的 `use crate::…` 照样编译通过，`clippy`/`fmt` 也全绿——只有把
//! "哪一层可以引用哪一层"写下来并断言，它才会在越界的那一次提交里失败。
//!
//! 允许的方向（与 `src/lib.rs` 的模块声明一致）：
//!
//! ```text
//! app ──▶ {filesystem, process} ──▶ {connect, wire, rest, cors, compress} ──▶ {auth, paths, init, ...}
//!                    └──────────────▶ generated（生成类型，谁都可以用，它自己不引用任何东西）
//! ```
//!
//! 本文件只断言"禁止的引用不存在"，不检查同层内部的顺序；新增一层时同时加一行
//! `GATES`，否则它不受任何约束。
//!
//! **`cargo test` 必须带 `tests/` 目标**：`--lib` / `--bins` 会跳过本文件，
//! 规则会静默失效（仓库里已有先例：`make hypervisor-test` 传 `--lib --bins`，
//! 它自己的 `tests/integration.rs` 就因此不被该目标执行）。

use std::{collections::BTreeSet, fs, path::Path, path::PathBuf};

/// `(说明, 归属模块, 禁止引用的模块)`。
const GATES: &[(&str, &str, &[&str])] = &[
    ("filesystem 不得引用 process", "filesystem", &["process"]),
    ("process 不得引用 filesystem", "process", &["filesystem"]),
    (
        "协议/工具层不得引用 app 或领域层",
        "connect",
        &["app", "filesystem", "process"],
    ),
    (
        "wire 不得引用 app 或领域层",
        "wire",
        &["app", "filesystem", "process"],
    ),
    (
        "rest 不得引用 app 或领域层",
        "rest",
        &["app", "filesystem", "process"],
    ),
    (
        "cors 不得引用 app 或领域层",
        "cors",
        &["app", "filesystem", "process"],
    ),
    (
        "compress 不得引用 app 或领域层",
        "compress",
        &["app", "filesystem", "process"],
    ),
    (
        "auth 不得引用 app 或领域层",
        "auth",
        &["app", "filesystem", "process"],
    ),
    (
        "paths 不得引用 app 或领域层",
        "paths",
        &["app", "filesystem", "process"],
    ),
    (
        "init 不得引用 app 或领域层",
        "init",
        &["app", "filesystem", "process"],
    ),
    (
        "logging 不得引用 app 或领域层",
        "logging",
        &["app", "filesystem", "process"],
    ),
    (
        "version 不得引用 app 或领域层",
        "version",
        &["app", "filesystem", "process"],
    ),
    (
        "生成的 protobuf 类型不得反向引用手写模块",
        "generated",
        &[
            "app",
            "auth",
            "compress",
            "connect",
            "cors",
            "filesystem",
            "init",
            "logging",
            "paths",
            "process",
            "rest",
            "version",
            "wire",
        ],
    ),
];

/// 收集 `src/` 下的全部 Rust 源文件。
fn rust_sources(dir: &Path, found: &mut Vec<PathBuf>) {
    for entry in fs::read_dir(dir).expect("read source directory") {
        let path = entry.expect("read directory entry").path();
        if path.is_dir() {
            rust_sources(&path, found);
        } else if path.extension().is_some_and(|extension| extension == "rs") {
            found.push(path);
        }
    }
}

/// 去掉行注释、块注释与字符串字面量，避免注释/文案里的模块路径造成误判。
fn strip_comments_and_strings(source: &str) -> String {
    let mut out = String::with_capacity(source.len());
    let mut chars = source.chars().peekable();
    let mut in_line_comment = false;
    let mut in_block_comment = false;
    let mut in_string = false;

    while let Some(character) = chars.next() {
        if in_line_comment {
            if character == '\n' {
                in_line_comment = false;
                out.push(character);
            }
            continue;
        }
        if in_block_comment {
            if character == '*' && chars.peek() == Some(&'/') {
                chars.next();
                in_block_comment = false;
            }
            continue;
        }
        if in_string {
            if character == '\\' {
                chars.next();
                continue;
            }
            if character == '"' {
                in_string = false;
            }
            continue;
        }
        match character {
            '/' if chars.peek() == Some(&'/') => {
                chars.next();
                in_line_comment = true;
            }
            '/' if chars.peek() == Some(&'*') => {
                chars.next();
                in_block_comment = true;
            }
            '"' => in_string = true,
            _ => out.push(character),
        }
    }
    out
}

/// 找出源码里引用到的 `crate::<module>`，包含 `use crate::{a, b::c}` 这种分组写法。
fn referenced_modules(source: &str) -> BTreeSet<String> {
    let cleaned = strip_comments_and_strings(source);
    let bytes: Vec<char> = cleaned.chars().collect();
    let mut modules = BTreeSet::new();
    let mut index = 0;

    while index < bytes.len() {
        // 定位 `crate` 且前一个字符不是标识符字符（避免匹配 xcrate）。
        if !cleaned[index..].starts_with("crate") {
            index += 1;
            continue;
        }
        let previous_is_ident = index
            .checked_sub(1)
            .and_then(|previous| bytes.get(previous))
            .is_some_and(|character| character.is_alphanumeric() || *character == '_');
        if previous_is_ident {
            index += 5;
            continue;
        }

        let mut cursor = index + "crate".len();
        while bytes.get(cursor).is_some_and(|c| c.is_whitespace()) {
            cursor += 1;
        }
        if !cleaned[cursor..].starts_with("::") {
            index += 5;
            continue;
        }
        cursor += 2;
        while bytes.get(cursor).is_some_and(|c| c.is_whitespace()) {
            cursor += 1;
        }

        if bytes.get(cursor) == Some(&'{') {
            // 分组导入：取每个条目的首段标识符。
            let mut depth = 0_usize;
            let mut entry = String::new();
            let mut entries: Vec<String> = Vec::new();
            while let Some(character) = bytes.get(cursor) {
                match character {
                    '{' => {
                        depth += 1;
                        if depth == 1 {
                            cursor += 1;
                            continue;
                        }
                    }
                    '}' => {
                        depth -= 1;
                        if depth == 0 {
                            entries.push(entry.clone());
                            break;
                        }
                    }
                    ',' if depth == 1 => {
                        entries.push(std::mem::take(&mut entry));
                    }
                    _ => {}
                }
                if depth >= 1 {
                    entry.push(*character);
                }
                cursor += 1;
            }
            for candidate in entries {
                let name: String = candidate
                    .trim()
                    .chars()
                    .take_while(|character| character.is_alphanumeric() || *character == '_')
                    .collect();
                if !name.is_empty() && name != "self" {
                    modules.insert(name);
                }
            }
            index = cursor;
            continue;
        }

        let name: String = bytes[cursor..]
            .iter()
            .take_while(|character| character.is_alphanumeric() || **character == '_')
            .collect();
        if !name.is_empty() {
            modules.insert(name);
        }
        index = cursor;
    }

    modules
}

/// 文件归属的顶层模块（`src/lib.rs` / `src/main.rs` 返回 None）。
fn owner_module(path: &Path) -> Option<String> {
    let relative = path.strip_prefix(path.ancestors().nth(1).unwrap()).ok()?;
    let _ = relative;
    let components: Vec<String> = path
        .components()
        .map(|component| component.as_os_str().to_string_lossy().to_string())
        .collect();
    let index = components.iter().position(|component| component == "src")?;
    let first = components.get(index + 1)?;
    let name = first.trim_end_matches(".rs").to_string();
    if name == "lib" || name == "main" {
        None
    } else {
        Some(name)
    }
}

// 验证源码树里的模块引用方向：禁止的引用一旦出现，本用例失败并指出文件。
#[test]
fn layer_rule_holds_for_the_source_tree() {
    let mut sources = Vec::new();
    rust_sources(
        Path::new(env!("CARGO_MANIFEST_DIR")).join("src").as_path(),
        &mut sources,
    );
    assert!(!sources.is_empty(), "no sources found");

    let mut violations: Vec<String> = Vec::new();
    for path in &sources {
        let Some(owner) = owner_module(path) else {
            continue;
        };
        let source = fs::read_to_string(path).expect("read source file");
        let referenced = referenced_modules(&source);
        for (description, gate_owner, forbidden) in GATES {
            if *gate_owner != owner {
                continue;
            }
            for name in *forbidden {
                if referenced.contains(*name) {
                    violations.push(format!(
                        "{description}: {} 引用了 crate::{name}",
                        path.display()
                    ));
                }
            }
        }
    }

    assert!(
        violations.is_empty(),
        "层规则被违反（{} 处）：\n{}",
        violations.len(),
        violations.join("\n")
    );
}
