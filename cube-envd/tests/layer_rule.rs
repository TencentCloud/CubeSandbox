// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! The layer rule, checked against the source tree:
//!
//! ```text
//! app -> {filesystem, process} -> platform -> protocol -> compat
//! ```
//!
//! Nothing else can check it. `pub(crate)` is reachable from every sibling
//! module and item visibility carries no direction, so an edge pointing the
//! wrong way compiles, and `cargo test`, `clippy` and `fmt` all stay green.
//! Reading the sources is deliberate: a module *path* is not something the type
//! system or the lints ever see.
//!
//! It lives in `tests/` rather than `src/` so the search space is exactly
//! `src/`: the forbidden paths below would otherwise match this file itself.
//!
//! This file is only as good as the command that runs it, so the requirement
//! belongs here, next to the code it constrains: **the suite must be invoked as
//! a plain `cargo test`.** `--lib` / `--bins` restrict the target set and would
//! skip `tests/` entirely, leaving the rule unchecked while `cargo test`,
//! `clippy` and `fmt` all stay green — exactly the failure mode this file
//! exists to prevent. (There is precedent for the mistake: `make
//! hypervisor-test` passes `--lib --bins`, and `hypervisor/tests/integration.rs`
//! is consequently not run by it but by a separate workflow.) If the cube-envd
//! CI command ever gains target flags, add `--tests` in the same change.
//!
//! Adding a layer means adding a row to `GATES` and a row to the table in the
//! PR description — if it is not in `GATES`, nothing enforces it.

use std::fs;
use std::path::{Path, PathBuf};

/// `(what is being checked, top-level modules it applies to, paths they must
/// not name)`.
const GATES: &[(&str, &[&str], &[&str])] = &[
    (
        "a layer below app/ reaches back into it",
        &["filesystem", "process", "platform", "protocol", "compat"],
        &["crate::app"],
    ),
    (
        "filesystem reaches into process",
        &["filesystem"],
        &["crate::process"],
    ),
    (
        "process reaches into filesystem",
        &["process"],
        &["crate::filesystem"],
    ),
    (
        "platform/protocol/compat reach into a domain",
        &["platform", "protocol", "compat"],
        &["crate::filesystem", "crate::process"],
    ),
    (
        "protocol or compat reaches up into platform",
        &["protocol", "compat"],
        &["crate::platform"],
    ),
    (
        "compat reaches up into protocol",
        &["compat"],
        &["crate::protocol"],
    ),
];

fn rust_files(dir: &Path, found: &mut Vec<PathBuf>) {
    for entry in fs::read_dir(dir).expect("read source directory") {
        let path = entry.expect("read directory entry").path();
        if path.is_dir() {
            rust_files(&path, found);
        } else if path.extension().is_some_and(|ext| ext == "rs") {
            found.push(path);
        }
    }
}

/// 1-based line and column of a byte offset, so the failure can be opened
/// directly.
fn line_col(text: &str, at: usize) -> (usize, usize) {
    let line = text[..at].matches('\n').count() + 1;
    let col = at - text[..at].rfind('\n').map_or(0, |nl| nl + 1) + 1;
    (line, col)
}

/// Offsets of every whole-segment occurrence of `needle`: `crate::app` matches
/// `crate::app::state` but not `crate::application`.
fn segments(text: &str, needle: &str) -> Vec<usize> {
    let mut hits = Vec::new();
    let mut from = 0;
    while let Some(offset) = text[from..].find(needle) {
        let at = from + offset;
        let boundary = match text[at + needle.len()..].chars().next() {
            Some(c) => !(c.is_alphanumeric() || c == '_'),
            None => true,
        };
        if boundary {
            hits.push(at);
        }
        from = at + needle.len();
    }
    hits
}

#[test]
fn layer_rule_holds() {
    let src = Path::new(env!("CARGO_MANIFEST_DIR")).join("src");
    let mut violations: Vec<String> = Vec::new();

    for (what, owners, forbidden) in GATES {
        for owner in *owners {
            let mut files = Vec::new();
            rust_files(&src.join(owner), &mut files);
            for file in files {
                let text = fs::read_to_string(&file).expect("read source file");
                for path in *forbidden {
                    for at in segments(&text, path) {
                        let (line, col) = line_col(&text, at);
                        violations.push(format!(
                            "src/{}:{line}:{col}: {path} — {what}",
                            file.strip_prefix(&src).unwrap_or(&file).display(),
                        ));
                    }
                }
            }
        }
    }

    assert!(
        violations.is_empty(),
        "the layer rule (app -> {{filesystem, process}} -> platform -> protocol \
         -> compat) is violated by {} reference(s):\n  {}",
        violations.len(),
        violations.join("\n  "),
    );
}
