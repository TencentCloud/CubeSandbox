// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use http::HeaderMap;

/// The file handler uses the first header value and Go's permissive quality
/// parser: unknown parameters and unparseable qualities leave the prior value.
pub(crate) fn coding(value: &str) -> (String, f64) {
    let mut parts = value.trim().split(';');
    let name = parts.next().unwrap_or("").trim().to_ascii_lowercase();
    let mut quality = 1.0;
    for parameter in parts {
        let parameter = parameter.trim();
        if parameter
            .get(..2)
            .is_some_and(|v| v.eq_ignore_ascii_case("q="))
        {
            if let Ok(value) = parameter[2..].parse() {
                quality = value;
            }
        }
    }
    (name, quality)
}

pub(crate) fn negotiate(headers: &HeaderMap) -> Result<bool, &'static str> {
    let value = headers
        .get("accept-encoding")
        .and_then(|v| v.to_str().ok())
        .unwrap_or("");
    let mut entries: Vec<_> = value.split(',').map(coding).collect();
    let explicit_identity = entries
        .iter()
        .any(|(name, q)| name == "identity" && *q != 0.0);
    let rejected = entries
        .iter()
        .any(|(name, q)| *q == 0.0 && (name == "identity" || (name == "*" && !explicit_identity)));
    entries.sort_by(|a, b| b.1.partial_cmp(&a.1).unwrap_or(std::cmp::Ordering::Equal));
    let selected = entries
        .iter()
        .filter(|(_, q)| *q != 0.0)
        .find_map(|(name, _)| match name.as_str() {
            "gzip" => Some(true),
            "identity" => Some(false),
            "*" => Some(rejected),
            _ => None,
        });
    let selected = selected
        .or(if rejected { None } else { Some(false) })
        .ok_or("error parsing Accept-Encoding: no acceptable encoding found, supported: [gzip]")?;
    let conditional = ["range", "if-modified-since", "if-none-match", "if-range"]
        .iter()
        .any(|name| headers.get(*name).is_some_and(|v| !v.is_empty()));
    if conditional {
        if rejected {
            return Err("identity encoding not acceptable for Range or conditional request");
        }
        Ok(false)
    } else {
        Ok(selected)
    }
}
