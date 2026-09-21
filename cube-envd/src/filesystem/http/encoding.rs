// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Accept-Encoding / content-encoding negotiation.
//!
//! 1:1 mirror of the same logic in upstream envd's `encoding.go`, which owns
//! the request half of the download pipeline. Everything here is pure so the
//! two 406 exits the handler emits stay wire-identical without I/O.
//!
//! cube-envd only ever *serves* identity (the CubeProxy applies gzip to text
//! types); the parser still mirrors upstream because a `Range`/conditional
//! request with an unacceptable identity must be refused with the same 406
//! upstream emits — including its `supported: [gzip]` message.

/// Content-encodings the server advertises, most preferred first. Mirror of
/// upstream `SupportedEncodings` — the parser uses it to resolve `*` and to
/// build the 406 message above, which is why it must stay `["gzip"]` even
/// though responses are identity-only (see module doc).
pub const SUPPORTED_ENCODINGS: [&str; 1] = ["gzip"];

/// An encoding the client may receive. cube-envd serves identity only; the
/// variants are exercised by the parser's unit tests.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Encoding {
    Identity,
    Gzip,
}

#[derive(Debug)]
struct EncodingWithQuality {
    encoding: String,
    quality: f64,
}

/// `parseEncodingWithQuality`: trim, split a `;`-parameter list, read `q=`
/// (case-insensitive; a malformed float is silently ignored, default 1.0),
/// lowercase the coding name.
fn parse_encoding_with_quality(value: &str) -> EncodingWithQuality {
    let mut value = value.trim();
    let mut quality = 1.0;
    if let Some(idx) = value.find(';') {
        let params = &value[idx + 1..];
        value = value[..idx].trim();
        for param in params.split(';') {
            let param = param.trim();
            if let Some(q) = param.strip_prefix("q=").or_else(|| {
                // encoding.go compares lowercased prefixes.
                let lower = param.to_ascii_lowercase();
                lower.strip_prefix("q=").map(|_| &param[2..])
            }) {
                if let Ok(v) = q.trim().parse::<f64>() {
                    quality = v;
                }
            }
        }
    }
    EncodingWithQuality {
        encoding: value.to_ascii_lowercase(),
        quality,
    }
}

fn is_supported_encoding(encoding: &str) -> bool {
    SUPPORTED_ENCODINGS.contains(&encoding.to_ascii_lowercase().as_str())
}

/// Parse the full header once and derive both outputs upstream computes from it.
fn parse_accept_encoding_parts(header: &str) -> (Vec<EncodingWithQuality>, bool) {
    if header.is_empty() {
        return (Vec::new(), false);
    }
    let encodings: Vec<EncodingWithQuality> =
        header.split(',').map(parse_encoding_with_quality).collect();

    // RFC 7231 §5.3.4: identity is acceptable unless excluded by
    // `identity;q=0` or `*;q=0` without a more specific identity entry with q>0.
    let mut identity_rejected = false;
    let mut identity_explicitly_accepted = false;
    let mut wildcard_rejected = false;
    for eq in &encodings {
        match eq.encoding.as_str() {
            "identity" => {
                if eq.quality == 0.0 {
                    identity_rejected = true;
                } else {
                    identity_explicitly_accepted = true;
                }
            }
            "*" => {
                if eq.quality == 0.0 {
                    wildcard_rejected = true;
                }
            }
            _ => {}
        }
    }
    if wildcard_rejected && !identity_explicitly_accepted {
        identity_rejected = true;
    }
    (encodings, identity_rejected)
}

/// `parseAcceptEncoding` — best acceptable encoding, or `Err` (→ the handler's
/// 406 without Vary) when identity is rejected and no supported coding is
/// acceptable. `""` header → identity.
///
/// Ordering: encoding.go sorts by quality with Go's unstable `sort.Slice`,
/// which leaves same-quality order undefined. We stable-sort so the client's
/// declaration order wins (RFC semantics); the conformance fixtures avoid
/// same-quality mixed codings, so both stay equivalent where it matters.
pub fn parse_accept_encoding(header: &str) -> Result<Encoding, ()> {
    if header.is_empty() {
        return Ok(Encoding::Identity);
    }
    let (mut encodings, identity_rejected) = parse_accept_encoding_parts(header);
    encodings.sort_by(|a, b| {
        b.quality
            .partial_cmp(&a.quality)
            .unwrap_or(std::cmp::Ordering::Equal)
    });

    for eq in &encodings {
        if eq.quality == 0.0 {
            continue;
        }
        match eq.encoding.as_str() {
            "identity" => return Ok(Encoding::Identity),
            "*" => {
                // Upstream encoding.go answers the `*` + identity-rejected
                // case only when the server advertises a non-empty supported
                // list (`len(server) > 0`). cube-envd's list is the constant
                // SUPPORTED_ENCODINGS == ["gzip"], which is never empty, so
                // the condition is constant and elided.
                return if identity_rejected {
                    Ok(Encoding::Gzip)
                } else {
                    Ok(Encoding::Identity)
                };
            }
            other => {
                if is_supported_encoding(other) {
                    return Ok(Encoding::Gzip);
                }
            }
        }
    }
    if !identity_rejected {
        return Ok(Encoding::Identity);
    }
    Err(())
}

/// `isIdentityAcceptable` — the Range/conditional-request gate (406 with Vary).
pub fn is_identity_acceptable(header: &str) -> bool {
    let (_, identity_rejected) = parse_accept_encoding_parts(header);
    !identity_rejected
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn ae_empty_header_is_identity() {
        assert_eq!(parse_accept_encoding("").unwrap(), Encoding::Identity);
    }

    #[test]
    fn ae_gzip_returns_gzip() {
        assert_eq!(parse_accept_encoding("gzip").unwrap(), Encoding::Gzip);
        assert_eq!(parse_accept_encoding("GZIP").unwrap(), Encoding::Gzip);
        assert_eq!(parse_accept_encoding("gzip;q=1.0").unwrap(), Encoding::Gzip);
    }

    #[test]
    fn ae_gzip_among_multiple() {
        assert_eq!(
            parse_accept_encoding("deflate, gzip, br").unwrap(),
            Encoding::Gzip
        );
    }

    #[test]
    fn ae_identity_explicit() {
        assert_eq!(
            parse_accept_encoding("identity").unwrap(),
            Encoding::Identity
        );
    }

    #[test]
    fn ae_gzip_q0_falls_to_identity() {
        assert_eq!(
            parse_accept_encoding("gzip;q=0").unwrap(),
            Encoding::Identity
        );
    }

    #[test]
    fn ae_identity_q0_rejects() {
        assert!(parse_accept_encoding("identity;q=0").is_err());
    }

    #[test]
    fn ae_wildcard_q0_rejects() {
        assert!(parse_accept_encoding("*;q=0").is_err());
    }

    #[test]
    fn ae_wildcard_alone_is_identity() {
        assert_eq!(parse_accept_encoding("*").unwrap(), Encoding::Identity);
    }

    #[test]
    fn ae_wildcard_q1_after_gzip_q0_prefers_identity() {
        // gzip is rejected; `*` covers identity at q=1.
        assert_eq!(
            parse_accept_encoding("gzip;q=0, *;q=1").unwrap(),
            Encoding::Identity
        );
    }

    #[test]
    fn ae_identity_q0_wildcard_q1_picks_gzip() {
        assert_eq!(
            parse_accept_encoding("identity;q=0, *;q=1").unwrap(),
            Encoding::Gzip
        );
    }

    #[test]
    fn ae_unsupported_encoding_falls_to_identity() {
        assert_eq!(parse_accept_encoding("br").unwrap(), Encoding::Identity);
        assert_eq!(
            parse_accept_encoding("deflate, br").unwrap(),
            Encoding::Identity
        );
    }

    #[test]
    fn ae_malformed_quality_ignored() {
        // `gzip;q=abc` parses as q=1 (float parse failure is swallowed).
        assert_eq!(parse_accept_encoding("gzip;q=abc").unwrap(), Encoding::Gzip);
    }

    #[test]
    fn ae_qvalue_case_insensitive_param() {
        // GZIP at q=0.5 loses to the wildcard at q=1; identity is not rejected.
        assert_eq!(
            parse_accept_encoding("GZIP;Q=0.5, *").unwrap(),
            Encoding::Identity
        );
    }

    #[test]
    fn ae_bad_header_garbage_is_identity() {
        // Nothing matches and identity is not rejected → identity.
        assert_eq!(
            parse_accept_encoding("zzz;q=0.9").unwrap(),
            Encoding::Identity
        );
    }

    #[test]
    fn ae_identity_acceptability() {
        assert!(is_identity_acceptable(""));
        assert!(is_identity_acceptable("gzip"));
        assert!(!is_identity_acceptable("identity;q=0"));
        assert!(!is_identity_acceptable("identity;q=0, *;q=1"));
        assert!(!is_identity_acceptable("*;q=0"));
        // Go keeps `identityRejected` once set even if a later `identity;q>0`
        // entry appears (that flag only rescues the wildcard path).
        assert!(!is_identity_acceptable("identity;q=0, identity;q=1, *;q=0"));
        // `identity;q>0` rescues `*;q=0` (wildcardRejected && !explicit → false).
        assert!(is_identity_acceptable("gzip;q=1, *;q=0, identity;q=0.5"));
    }
}
