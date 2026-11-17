// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! CORS middleware mirroring upstream envd's `withCORS` (`main.go:108-130`),
//! which configures `github.com/rs/cors` v1.11.1 with a permissive policy.
//!
//! Hand-written rather than `tower_http::cors` because rs/cors' observable
//! behavior differs from the tower layer in ways a caller can see:
//! - a preflight echoes `Access-Control-Request-Method` / `-Headers` instead
//!   of listing the configured policy (`handlePreflight`, cors.go:359-395);
//! - `Vary` is set on *every* response, including requests without an
//!   `Origin` (cors.go:337-344 / :406-411);
//! - a request whose method is not in the allowed set is answered without
//!   `Access-Control-Allow-Origin` instead of being rejected outright;
//! - a preflight is standalone: it answers 204 without reaching the handler.

use std::borrow::Cow;

use axum::extract::Request;
use axum::http::header::{
    HeaderName, ACCESS_CONTROL_ALLOW_HEADERS, ACCESS_CONTROL_ALLOW_METHODS,
    ACCESS_CONTROL_ALLOW_ORIGIN, ACCESS_CONTROL_EXPOSE_HEADERS, ACCESS_CONTROL_MAX_AGE,
    ACCESS_CONTROL_REQUEST_HEADERS, ACCESS_CONTROL_REQUEST_METHOD, ORIGIN, VARY,
};
use axum::http::{HeaderMap, HeaderValue, StatusCode};
use axum::middleware::Next;
use axum::response::{IntoResponse, Response};

/// `main.go:111-118` — the six methods upstream allows.
const ALLOWED_METHODS: [&str; 6] = ["HEAD", "GET", "POST", "PUT", "PATCH", "DELETE"];

/// `connectcors.ExposedHeaders()` (`Grpc-Status`, `Grpc-Message`,
/// `Grpc-Status-Details-Bin`) plus the three upstream appends, canonicalized
/// and joined once by rs/cors (cors.go:227-229).
pub const EXPOSED_HEADERS: &str =
    "Grpc-Status, Grpc-Message, Grpc-Status-Details-Bin, Location, Cache-Control, X-Content-Type-Options";

/// `main.go:36` — `maxAge = 2 * time.Hour`.
pub const MAX_AGE: &str = "7200";

pub const PREFLIGHT_VARY: &str =
    "Origin, Access-Control-Request-Method, Access-Control-Request-Headers";
pub const ACTUAL_VARY: &str = "Origin";

/// A preflight is an OPTIONS request carrying `Access-Control-Request-Method`;
/// everything else is an actual request (rs/cors `ServeHTTP`, cors.go:309).
pub fn is_preflight(method: &str, request_method: Option<&str>) -> bool {
    method == "OPTIONS" && request_method.is_some_and(|m| !m.is_empty())
}

fn is_method_allowed(method: &str) -> bool {
    // rs/cors always allows OPTIONS as a method, even on actual (non-preflight)
    // requests — `isMethodAllowed` returns true for OPTIONS before consulting
    // the configured set (cors.go:490-492), so a bare `OPTIONS /x` + Origin
    // still gets ACAO/Expose-Headers on its 405. OPTIONS is deliberately not
    // in ALLOWED_METHODS (it answers before the router either way).
    method == "OPTIONS" || ALLOWED_METHODS.contains(&method)
}

/// The CORS headers upstream adds to a response, in order. `Vary` is always
/// present; the rest appear only when the request is actually cross-origin and
/// its method is allowed.
pub fn cors_headers(
    method: &str,
    origin: Option<&str>,
    request_method: Option<&str>,
    request_headers: Option<&str>,
) -> Vec<(HeaderName, Cow<'static, str>)> {
    // An empty Origin header counts as no Origin at all (cors.go:350, :415).
    let origin = origin.filter(|o| !o.is_empty());
    if is_preflight(method, request_method) {
        // Unwrap is safe: a preflight always carries a non-empty method.
        let request_method = request_method.unwrap_or_default();
        let mut out = vec![(VARY, Cow::Borrowed(PREFLIGHT_VARY))];
        if origin.is_none() || !is_method_allowed(request_method) {
            return out;
        }
        out.push((ACCESS_CONTROL_ALLOW_ORIGIN, Cow::Borrowed("*")));
        out.push((
            ACCESS_CONTROL_ALLOW_METHODS,
            Cow::Owned(request_method.to_string()),
        ));
        if let Some(headers) = request_headers.filter(|h| !h.is_empty()) {
            out.push((
                ACCESS_CONTROL_ALLOW_HEADERS,
                Cow::Owned(headers.to_string()),
            ));
        }
        out.push((ACCESS_CONTROL_MAX_AGE, Cow::Borrowed(MAX_AGE)));
        out
    } else {
        let mut out = vec![(VARY, Cow::Borrowed(ACTUAL_VARY))];
        if origin.is_none() || !is_method_allowed(method) {
            return out;
        }
        out.push((ACCESS_CONTROL_ALLOW_ORIGIN, Cow::Borrowed("*")));
        out.push((
            ACCESS_CONTROL_EXPOSE_HEADERS,
            Cow::Borrowed(EXPOSED_HEADERS),
        ));
        out
    }
}

pub async fn middleware(request: Request, next: Next) -> Response {
    let (origin, request_method, request_headers) = {
        let head = request.headers();
        // An empty header value means "absent" to rs/cors (cors.go:350, :415).
        let value = |name: &HeaderName| {
            head.get(name)
                .and_then(|v| v.to_str().ok())
                .filter(|v| !v.is_empty())
                .map(str::to_string)
        };
        (
            value(&ORIGIN),
            value(&ACCESS_CONTROL_REQUEST_METHOD),
            value(&ACCESS_CONTROL_REQUEST_HEADERS),
        )
    };
    let preflight = is_preflight(request.method().as_str(), request_method.as_deref());
    let headers = cors_headers(
        request.method().as_str(),
        origin.as_deref(),
        request_method.as_deref(),
        request_headers.as_deref(),
    );
    if preflight {
        // Standalone answer (204): upstream stops the chain here so the
        // OPTIONS request never reaches handlers that have no route for it.
        let mut response = StatusCode::NO_CONTENT.into_response();
        apply(response.headers_mut(), headers);
        return response;
    }
    let mut response = next.run(request).await;
    apply(response.headers_mut(), headers);
    response
}

/// Merge the CORS headers into a response.
///
/// `Vary` is the one header that can differ: rs/cors writes it *before* the
/// handler runs, so a handler that sets its own `Vary` replaces it — upstream's
/// `/files` answers `Vary: Accept-Encoding` and never `..., Origin`
/// (`download.go:118` overwrites the middleware's value). Leaving an existing
/// `Vary` untouched reproduces that; every other CORS header is ours to set.
fn apply(target: &mut HeaderMap, headers: Vec<(HeaderName, Cow<'static, str>)>) {
    for (name, value) in headers {
        let parsed = match HeaderValue::from_str(&value) {
            Ok(v) => v,
            Err(_) => continue,
        };
        if name == VARY {
            if !target.contains_key(&VARY) {
                target.insert(&VARY, parsed);
            }
        } else {
            target.insert(&name, parsed);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn render(headers: &[(HeaderName, Cow<'static, str>)]) -> Vec<String> {
        headers
            .iter()
            .map(|(name, value)| format!("{name}: {value}"))
            .collect()
    }

    #[test]
    fn preflight_echoes_request_method_and_headers() {
        let headers = cors_headers("OPTIONS", Some("https://x.com"), Some("POST"), None);
        assert_eq!(
            render(&headers),
            vec![
                "vary: Origin, Access-Control-Request-Method, Access-Control-Request-Headers",
                "access-control-allow-origin: *",
                "access-control-allow-methods: POST",
                "access-control-max-age: 7200",
            ]
        );
        // rs/cors echoes Access-Control-Request-Headers verbatim, and omits the
        // header entirely when the request does not ask for any.
        let with_headers = cors_headers(
            "OPTIONS",
            Some("https://x.com"),
            Some("POST"),
            Some("content-type, x-access-token"),
        );
        assert!(render(&with_headers)
            .contains(&"access-control-allow-headers: content-type, x-access-token".to_string()));
        // No Expose-Headers on a preflight — upstream only sends it on actual
        // requests (verified against the Go baseline).
        assert!(!render(&with_headers).iter().any(|h| h.contains("expose")));
    }

    #[test]
    fn preflight_without_origin_or_with_disallowed_method_adds_only_vary() {
        let vary_only =
            vec!["vary: Origin, Access-Control-Request-Method, Access-Control-Request-Headers"];
        // No Origin: rs/cors aborts the preflight (cors.go:350).
        assert_eq!(
            render(&cors_headers("OPTIONS", None, Some("POST"), None)),
            vary_only
        );
        // Method outside the configured six (cors.go:360).
        assert_eq!(
            render(&cors_headers(
                "OPTIONS",
                Some("https://x.com"),
                Some("TRACE"),
                None
            )),
            vary_only
        );
        // Every configured method is accepted.
        for method in ALLOWED_METHODS {
            let headers = cors_headers("OPTIONS", Some("https://x.com"), Some(method), None);
            assert!(render(&headers)
                .iter()
                .any(|h| h == &format!("access-control-allow-methods: {method}")));
        }
    }

    #[test]
    fn actual_request_exposes_the_six_headers() {
        let headers = cors_headers("GET", Some("https://x.com"), None, None);
        assert_eq!(
            render(&headers),
            vec![
                "vary: Origin".to_string(),
                "access-control-allow-origin: *".to_string(),
                format!("access-control-expose-headers: {EXPOSED_HEADERS}"),
            ]
        );
    }

    #[test]
    fn actual_request_without_origin_or_with_disallowed_method_adds_only_vary() {
        assert_eq!(
            render(&cors_headers("GET", None, None, None)),
            vec!["vary: Origin"]
        );
        // An empty Origin header counts as absent.
        assert_eq!(
            render(&cors_headers("GET", Some(""), None, None)),
            vec!["vary: Origin"]
        );
        // OPTIONS without Access-Control-Request-Method is an actual request.
        // rs/cors treats OPTIONS as always allowed (cors.go:490-492), so it
        // gets the full header set — verified live against 0.5.13: upstream
        // answers 405 with ACAO `*` + Expose-Headers, never Vary-only.
        assert_eq!(
            render(&cors_headers("OPTIONS", Some("https://x.com"), None, None)),
            vec![
                "vary: Origin".to_string(),
                "access-control-allow-origin: *".to_string(),
                format!("access-control-expose-headers: {EXPOSED_HEADERS}"),
            ]
        );
        assert_eq!(
            render(&cors_headers("TRACE", Some("https://x.com"), None, None)),
            vec!["vary: Origin"]
        );
    }

    #[test]
    fn preflight_detection() {
        assert!(is_preflight("OPTIONS", Some("POST")));
        // No Access-Control-Request-Method: not a preflight...
        assert!(!is_preflight("OPTIONS", None));
        assert!(!is_preflight("OPTIONS", Some("")));
        // ...and neither is any other method.
        assert!(!is_preflight("GET", Some("POST")));
    }

    #[test]
    fn a_handler_owned_vary_wins() {
        // Upstream's CORS layer writes Vary before the handler runs, so a
        // handler that sets its own Vary replaces it: /files answers
        // `Vary: Accept-Encoding` alone, never `Accept-Encoding, Origin`.
        let mut headers = HeaderMap::new();
        headers.insert(&VARY, HeaderValue::from_static("Accept-Encoding"));
        apply(
            &mut headers,
            cors_headers("GET", Some("https://x.com"), None, None),
        );
        assert_eq!(
            headers.get(&VARY).and_then(|v| v.to_str().ok()),
            Some("Accept-Encoding")
        );
        assert_eq!(
            headers
                .get(&ACCESS_CONTROL_ALLOW_ORIGIN)
                .and_then(|v| v.to_str().ok()),
            Some("*")
        );
    }
}
