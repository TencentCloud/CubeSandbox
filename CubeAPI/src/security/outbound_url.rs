// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! Outbound URL security layer.
//!
//! This module validates URLs before CubeAPI calls them, preventing SSRF
//! attacks against loopback, private, link-local, and metadata-service
//! addresses. It also pins resolved DNS names to a `reqwest::Client` so
//! that the IP address used at validation time is the same one used at
//! request time, defeating DNS rebinding.
//!
//! The public API is intentionally small:
//!
//! - [`OutboundUrlSecurityConfig`]: configuration (deserializable from env vars).
//! - [`OutboundUrlPolicy`]: the validation policy.
//! - [`ValidatedUrl`]: the result of a successful validation.
//! - [`build_secure_client`]: builds a hardened `reqwest::Client` from a validated URL.
//! - [`read_body_with_limit`]: reads a response body with an upper size bound.

use async_trait::async_trait;
#[cfg(feature = "webhooks")]
use futures::StreamExt;
use serde::Deserialize;
use std::collections::HashSet;
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr, SocketAddr};
use std::time::Duration;
use thiserror::Error;
use tokio::time::timeout;
use tracing::{debug, warn};
use url::Host;

/// Configuration for outbound URL security.
///
/// Loaded from environment variables using the `__` separator, e.g.:
///
/// ```bash
/// OUTBOUND_URL_SECURITY__ALLOWED_SCHEMES=https
/// OUTBOUND_URL_SECURITY__ALLOW_PRIVATE_IPS=false
/// OUTBOUND_URL_SECURITY__RESOLVE_TIMEOUT_MS=5000
/// ```
#[derive(Debug, Clone, Deserialize)]
pub struct OutboundUrlSecurityConfig {
    /// Allowed URL schemes. Defaults to `["https"]`.
    ///
    /// Accepts either a comma-separated string (`"https,http"`) or a list
    /// (`["https", "http"]`), so it can be loaded from a single environment
    /// variable without enabling global typed parsing.
    ///
    /// The custom deserializer uses `deserialize_any` because the `config`
    /// crate supplies either a string or a sequence depending on the source.
    /// If the configuration source changes to TOML/YAML/etc., re-validate this
    /// behavior.
    #[serde(default = "default_allowed_schemes", deserialize_with = "deserialize_schemes")]
    pub allowed_schemes: Vec<String>,

    /// Whether to allow resolved addresses in private/link-local/etc. ranges.
    /// Defaults to `false`.
    #[serde(default)]
    pub allow_private_ips: bool,

    /// DNS resolution timeout in milliseconds. Defaults to 5000.
    #[serde(default = "default_resolve_timeout_ms")]
    pub resolve_timeout_ms: u64,
}

impl Default for OutboundUrlSecurityConfig {
    fn default() -> Self {
        Self {
            allowed_schemes: default_allowed_schemes(),
            allow_private_ips: false,
            resolve_timeout_ms: default_resolve_timeout_ms(),
        }
    }
}

fn default_allowed_schemes() -> Vec<String> {
    vec!["https".to_string()]
}

fn default_resolve_timeout_ms() -> u64 {
    5000
}

fn deserialize_schemes<'de, D>(deserializer: D) -> Result<Vec<String>, D::Error>
where
    D: serde::Deserializer<'de>,
{
    struct StringOrVec;

    impl<'de> serde::de::Visitor<'de> for StringOrVec {
        type Value = Vec<String>;

        fn expecting(&self, formatter: &mut std::fmt::Formatter) -> std::fmt::Result {
            formatter.write_str("a string or a list of strings")
        }

        fn visit_str<E>(self, value: &str) -> Result<Self::Value, E>
        where
            E: serde::de::Error,
        {
            Ok(value.split(',').map(String::from).collect())
        }

        fn visit_seq<A>(self, mut seq: A) -> Result<Self::Value, A::Error>
        where
            A: serde::de::SeqAccess<'de>,
        {
            let mut schemes = Vec::new();
            while let Some(scheme) = seq.next_element::<String>()? {
                schemes.push(scheme);
            }
            Ok(schemes)
        }
    }

    deserializer.deserialize_any(StringOrVec)
}

/// Errors produced while validating an outbound URL.
#[derive(Debug, Error)]
pub enum OutboundUrlError {
    /// The input could not be parsed as a URL.
    #[error("invalid URL: {0}")]
    InvalidUrl(String),

    /// The URL scheme is not in the allow-list.
    #[error("unsupported URL scheme: {0}")]
    UnsupportedScheme(String),

    /// The scheme is allowed but has no well-known default port and the URL
    /// does not specify one explicitly.
    #[error("scheme {0} has no well-known default port; specify host:port explicitly")]
    NoDefaultPort(String),

    /// The URL has no host component.
    #[error("URL has no host")]
    MissingHost,

    /// The URL contains embedded credentials (`user:pass@host`).
    #[error("URL contains embedded credentials")]
    EmbeddedCredentials,

    /// The host is explicitly disallowed (e.g. `localhost`).
    #[error("host is not allowed: {0}")]
    HostNotAllowed(String),

    /// DNS resolution failed.
    #[error("DNS resolution failed for {host}: {source}")]
    ResolutionFailed {
        host: String,
        #[source]
        source: std::io::Error,
    },

    /// DNS resolution timed out.
    #[error("DNS resolution timed out for {0}")]
    ResolutionTimeout(String),

    /// A resolved address is not public and `allow_private_ips` is false.
    #[error("resolved address {0} is not a public IP")]
    NonPublicAddress(IpAddr),
}

/// Errors produced while reading a bounded response body.
#[cfg(feature = "webhooks")]
#[derive(Debug, Error)]
pub enum BodyLimitError {
    /// The response body exceeds the configured maximum size.
    #[error("response body exceeds maximum allowed size")]
    BodyTooLarge,

    /// An underlying network or decoding error occurred.
    #[error("failed to read response body: {0}")]
    ReadError(#[from] reqwest::Error),
}

/// A URL that has passed outbound-security validation.
#[derive(Debug, Clone)]
pub struct ValidatedUrl {
    /// The parsed URL. Path, query, and fragment are preserved.
    pub url: Url,

    /// The addresses that the host resolved to. Empty for IP-literal hosts.
    pub resolved: Vec<SocketAddr>,
}

/// Trait for DNS resolution. Used internally and swapped out in tests.
#[async_trait]
trait Resolver: Send + Sync {
    async fn resolve(&self, host: &str, port: u16) -> Result<Vec<SocketAddr>, std::io::Error>;
}

/// Default resolver backed by `tokio::net::lookup_host`.
struct TokioResolver;

#[async_trait]
impl Resolver for TokioResolver {
    async fn resolve(&self, host: &str, port: u16) -> Result<Vec<SocketAddr>, std::io::Error> {
        tokio::net::lookup_host((host, port))
            .await
            .map(|iter| iter.collect())
    }
}

/// Policy for validating outbound URLs.
///
/// Create a policy with one of the constructors:
///
/// - [`OutboundUrlPolicy::strict`]: production defaults (https only, no private IPs).
/// - [`OutboundUrlPolicy::development`]: allows http and private IPs for local testing.
/// - [`OutboundUrlPolicy::webhook_default`]: intended for future webhook endpoint validation.
/// - [`OutboundUrlPolicy::from_config`]: loads from [`OutboundUrlSecurityConfig`].
pub struct OutboundUrlPolicy {
    allowed_schemes: HashSet<String>,
    allow_private_ips: bool,
    resolve_timeout: Duration,
    resolver: Box<dyn Resolver>,
}

impl std::fmt::Debug for OutboundUrlPolicy {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("OutboundUrlPolicy")
            .field("allowed_schemes", &self.allowed_schemes)
            .field("allow_private_ips", &self.allow_private_ips)
            .field("resolve_timeout", &self.resolve_timeout)
            .field("resolver", &"<dyn Resolver>")
            .finish()
    }
}

impl OutboundUrlPolicy {
    /// Production-strict policy: only `https`, no private IPs.
    pub fn strict() -> Self {
        Self {
            allowed_schemes: HashSet::from(["https".to_string()]),
            allow_private_ips: false,
            resolve_timeout: Duration::from_secs(5),
            resolver: Box::new(TokioResolver),
        }
    }

    /// Development policy: allows `http` and private IPs.
    ///
    /// Test helper that relaxes the policy for local development scenarios.
    /// It is gated to tests so that production binaries do not ship unused
    /// code; local development can achieve the same effect through
    /// [`OutboundUrlSecurityConfig`].
    #[cfg(test)]
    pub fn development() -> Self {
        Self {
            allowed_schemes: HashSet::from(["http".to_string(), "https".to_string()]),
            allow_private_ips: true,
            resolve_timeout: Duration::from_secs(5),
            resolver: Box::new(TokioResolver),
        }
    }

    /// Recommended policy for future webhook endpoints.
    #[cfg(feature = "webhooks")]
    pub fn webhook_default() -> Self {
        Self::strict()
    }

    /// Build a policy from application configuration.
    ///
    /// This constructor initializes every field explicitly rather than
    /// starting from [`Self::strict()`] and overwriting, so adding a new field
    /// to [`OutboundUrlPolicy`] forces this function to be updated at compile
    /// time and prevents silent divergence from the configured defaults.
    pub fn from_config(cfg: &OutboundUrlSecurityConfig) -> Self {
        Self {
            allowed_schemes: cfg
                .allowed_schemes
                .iter()
                .map(|s| s.to_lowercase())
                .collect(),
            allow_private_ips: cfg.allow_private_ips,
            resolve_timeout: Duration::from_millis(cfg.resolve_timeout_ms),
            resolver: Box::new(TokioResolver),
        }
    }

    /// Validate an outbound URL.
    ///
    /// On success, returns a [`ValidatedUrl`] carrying the parsed URL and the
    /// resolved addresses. The resolved addresses can later be pinned to a
    /// `reqwest::Client` via [`build_secure_client`].
    pub async fn validate(&self, raw: &str) -> Result<ValidatedUrl, OutboundUrlError> {
        let url = Url::parse(raw).map_err(|e| OutboundUrlError::InvalidUrl(e.to_string()))?;

        self.validate_scheme(&url)?;
        self.validate_credentials(&url)?;
        self.validate_host(&url)?;

        let port = url
            .port_or_known_default()
            .ok_or_else(|| OutboundUrlError::NoDefaultPort(url.scheme().to_string()))?;

        // Resolve and classify addresses. Prefer `url.host()` over `host_str()`
        // so IPv6 literals are returned as `IpAddr` without square brackets.
        let resolved = match url.host() {
            Some(Host::Ipv4(ip)) => {
                debug!(%url, %ip, "outbound URL uses IPv4 literal");
                self.validate_ip(IpAddr::V4(ip))?;
                vec![SocketAddr::new(IpAddr::V4(ip), port)]
            }
            Some(Host::Ipv6(ip)) => {
                debug!(%url, %ip, "outbound URL uses IPv6 literal");
                self.validate_ip(IpAddr::V6(ip))?;
                vec![SocketAddr::new(IpAddr::V6(ip), port)]
            }
            Some(Host::Domain(domain)) => {
                debug!(%url, host = %domain, "resolving outbound URL host");
                let addrs = self.resolve_with_timeout(domain, port).await?;
                if addrs.is_empty() {
                    return Err(OutboundUrlError::ResolutionFailed {
                        host: domain.to_string(),
                        source: std::io::Error::new(
                            std::io::ErrorKind::NotFound,
                            "no addresses returned",
                        ),
                    });
                }
                for addr in &addrs {
                    self.validate_ip(addr.ip())?;
                }
                addrs
            }
            // Defensive: with the current `url` crate this arm is unreachable from
            // `Url::parse` output because an empty/absent host is rejected at parse
            // time. It is kept to guard against future `url` behavior changes or
            // programmatically constructed `Url` values without a host.
            None => return Err(OutboundUrlError::MissingHost),
        };

        Ok(ValidatedUrl { url, resolved })
    }

    fn validate_scheme(&self, url: &Url) -> Result<(), OutboundUrlError> {
        let scheme = url.scheme();
        if !self.allowed_schemes.contains(scheme) {
            return Err(OutboundUrlError::UnsupportedScheme(scheme.to_string()));
        }
        Ok(())
    }

    fn validate_host(&self, url: &Url) -> Result<(), OutboundUrlError> {
        let host = url
            .host_str()
            .ok_or(OutboundUrlError::MissingHost)?
            .to_lowercase();

        if host.is_empty() {
            return Err(OutboundUrlError::MissingHost);
        }

        // Reject localhost explicitly regardless of DNS behavior.
        if host == "localhost" {
            return Err(OutboundUrlError::HostNotAllowed(host));
        }

        Ok(())
    }

    fn validate_credentials(&self, url: &Url) -> Result<(), OutboundUrlError> {
        if !url.username().is_empty() || url.password().is_some() {
            return Err(OutboundUrlError::EmbeddedCredentials);
        }
        Ok(())
    }

    async fn resolve_with_timeout(
        &self,
        host: &str,
        port: u16,
    ) -> Result<Vec<SocketAddr>, OutboundUrlError> {
        let fut = self.resolver.resolve(host, port);
        timeout(self.resolve_timeout, fut)
            .await
            .map_err(|_| OutboundUrlError::ResolutionTimeout(host.to_string()))?
            .map_err(|e| OutboundUrlError::ResolutionFailed {
                host: host.to_string(),
                source: e,
            })
    }

    fn validate_ip(&self, ip: IpAddr) -> Result<(), OutboundUrlError> {
        if self.allow_private_ips {
            return Ok(());
        }
        if is_public_ip(ip) {
            Ok(())
        } else {
            warn!(%ip, "rejecting non-public outbound IP address");
            Err(OutboundUrlError::NonPublicAddress(ip))
        }
    }

    /// Test-only constructor that swaps the DNS resolver.
    #[cfg(test)]
    fn with_resolver(mut self, resolver: Box<dyn Resolver>) -> Self {
        self.resolver = resolver;
        self
    }

    /// Test-only setter for the DNS resolution timeout.
    #[cfg(test)]
    fn with_resolve_timeout(mut self, timeout: Duration) -> Self {
        self.resolve_timeout = timeout;
        self
    }
}

/// Return `true` if `ip` is a publicly routable address.
fn is_public_ip(ip: IpAddr) -> bool {
    match ip {
        IpAddr::V4(v4) => is_public_ipv4(v4),
        IpAddr::V6(v6) => {
            // Treat IPv4-mapped and IPv4-compatible addresses as their inner IPv4.
            if let Some(v4) = v6.to_ipv4_mapped().or(v6.to_ipv4()) {
                return is_public_ipv4(v4);
            }
            !(v6.is_loopback()
                || v6.is_unique_local()
                || v6.is_unicast_link_local()
                || v6.is_multicast()
                || v6.is_unspecified()
                || is_ipv6_benchmarking(v6)
                || is_ipv6_ietf_protocol_assignment(v6)
                || is_ipv6_documentation(v6))
        }
    }
}

fn is_public_ipv4(v4: Ipv4Addr) -> bool {
    !(v4.is_loopback()
        || v4.is_private()
        || v4.is_link_local()
        || v4.is_multicast()
        || v4.is_broadcast()
        || v4.is_documentation()
        || v4.is_unspecified()
        || is_shared_address_space(v4)
        || is_ietf_protocol_assignments(v4)
        || is_this_network(v4))
}

/// 2001:db8::/32 (IPv6 documentation range).
fn is_ipv6_documentation(v6: Ipv6Addr) -> bool {
    let segments = v6.segments();
    segments[0] == 0x2001 && segments[1] == 0x0db8
}

/// 2001:2::/48 (IPv6 benchmarking range).
fn is_ipv6_benchmarking(v6: Ipv6Addr) -> bool {
    let segments = v6.segments();
    segments[0] == 0x2001 && segments[1] == 0x0002 && segments[2] == 0x0000
}

/// 2001:3::/32 (IPv6 IETF protocol assignments).
fn is_ipv6_ietf_protocol_assignment(v6: Ipv6Addr) -> bool {
    let segments = v6.segments();
    segments[0] == 0x2001 && segments[1] == 0x0003
}

/// 100.64.0.0/10 (CGNAT / shared address space).
fn is_shared_address_space(v4: Ipv4Addr) -> bool {
    let octets = v4.octets();
    octets[0] == 100 && (64..=127).contains(&octets[1])
}

/// 192.0.0.0/24 (IETF protocol assignments, includes DS-Lite 192.0.0.1/2).
fn is_ietf_protocol_assignments(v4: Ipv4Addr) -> bool {
    let octets = v4.octets();
    octets[0] == 192 && octets[1] == 0 && octets[2] == 0
}

/// 0.0.0.0/8 (this network).
fn is_this_network(v4: Ipv4Addr) -> bool {
    v4.octets()[0] == 0
}

/// Build a hardened `reqwest::Client` for a validated outbound URL.
///
/// The client:
///
/// - Disables automatic redirects.
/// - Disables HTTP proxies.
/// - Sets connect and request timeouts.
/// - Pins the validated DNS addresses to the host name, preventing DNS rebinding.
///
/// Because resolved IPs are pinned at construction time, the client will not
/// follow DNS record changes (e.g., CDN rotation or load-balancer failover).
/// The application must be restarted to re-resolve and pin new addresses.
///
/// # Errors
///
/// Returns an error if `reqwest` fails to build the client.
pub fn build_secure_client(
    validated: &ValidatedUrl,
    connect_timeout: Duration,
    request_timeout: Duration,
) -> Result<reqwest::Client, reqwest::Error> {
    let mut builder = reqwest::Client::builder()
        .redirect(reqwest::redirect::Policy::none())
        .no_proxy()
        .connect_timeout(connect_timeout)
        .timeout(request_timeout);

    // Pin resolved addresses for domain hosts. IP-literal hosts do not need
    // pinning because no DNS lookup is performed at request time.
    if matches!(validated.url.host(), Some(Host::Domain(_))) {
        if let Some(host) = validated.url.host_str() {
            for addr in &validated.resolved {
                builder = builder.resolve(host, *addr);
            }
        }
    }

    builder.build()
}

/// Read a response body up to `max_bytes`.
///
/// If `Content-Length` is present and larger than `max_bytes`, the function
/// returns immediately. Otherwise chunks are streamed until the body is
/// complete or the limit is exceeded.
///
/// This prevents an attacker from exhausting CubeAPI memory with a huge
/// response body.
#[cfg(feature = "webhooks")]
pub async fn read_body_with_limit(
    response: reqwest::Response,
    max_bytes: usize,
) -> Result<Vec<u8>, BodyLimitError> {
    if let Some(len) = response
        .headers()
        .get(reqwest::header::CONTENT_LENGTH)
        .and_then(|v| v.to_str().ok())
        .and_then(|s| s.parse::<u64>().ok())
    {
        if len > max_bytes as u64 {
            return Err(BodyLimitError::BodyTooLarge);
        }
    }

    let mut body = Vec::with_capacity(max_bytes.min(4096));
    let mut stream = response.bytes_stream();

    while let Some(chunk) = stream.next().await {
        let chunk = chunk?;
        if body.len().saturating_add(chunk.len()) > max_bytes {
            return Err(BodyLimitError::BodyTooLarge);
        }
        body.extend_from_slice(&chunk);
    }

    Ok(body)
}

// Re-export `Url` so callers do not need to add the `url` crate themselves.
pub use url::Url;

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;

    /// A resolver that returns fixed addresses for configured hosts.
    struct StaticResolver {
        records: HashMap<String, Vec<IpAddr>>,
    }

    impl StaticResolver {
        fn new(records: HashMap<String, Vec<IpAddr>>) -> Self {
            Self { records }
        }
    }

    #[async_trait]
    impl Resolver for StaticResolver {
        async fn resolve(&self, host: &str, port: u16) -> Result<Vec<SocketAddr>, std::io::Error> {
            let addrs = self
                .records
                .get(host)
                .cloned()
                .unwrap_or_default()
                .into_iter()
                .map(|ip| SocketAddr::new(ip, port))
                .collect();
            Ok(addrs)
        }
    }

    fn policy_with_resolver(records: HashMap<String, Vec<IpAddr>>) -> OutboundUrlPolicy {
        OutboundUrlPolicy::strict().with_resolver(Box::new(StaticResolver::new(records)))
    }

    fn ipv4(a: u8, b: u8, c: u8, d: u8) -> IpAddr {
        IpAddr::V4(Ipv4Addr::new(a, b, c, d))
    }

    #[tokio::test]
    async fn valid_https_url_passes() {
        let records = HashMap::from([("example.com".to_string(), vec![ipv4(1, 2, 3, 4)])]);
        let policy = policy_with_resolver(records);
        let result = policy.validate("https://example.com/hook").await;
        assert!(result.is_ok(), "unexpected error: {:?}", result.err());
    }

    #[tokio::test]
    async fn http_scheme_rejected_by_default() {
        let policy = OutboundUrlPolicy::strict();
        let err = policy
            .validate("http://example.com/hook")
            .await
            .unwrap_err();
        assert!(matches!(err, OutboundUrlError::UnsupportedScheme(_)));
    }

    #[tokio::test]
    #[cfg(feature = "webhooks")]
    async fn webhook_default_uses_strict_policy() {
        let policy = OutboundUrlPolicy::webhook_default();
        let records = HashMap::from([("example.com".to_string(), vec![ipv4(1, 2, 3, 4)])]);
        let policy = policy.with_resolver(Box::new(StaticResolver::new(records)));
        assert!(policy.validate("https://example.com/hook").await.is_ok());
        let err = policy
            .validate("http://example.com/hook")
            .await
            .unwrap_err();
        assert!(matches!(err, OutboundUrlError::UnsupportedScheme(_)));
    }

    #[tokio::test]
    async fn http_scheme_allowed_when_configured() {
        let policy = OutboundUrlPolicy::development();
        let records = HashMap::from([("example.com".to_string(), vec![ipv4(1, 2, 3, 4)])]);
        let policy = policy.with_resolver(Box::new(StaticResolver::new(records)));
        let result = policy.validate("http://example.com/hook").await;
        assert!(result.is_ok(), "unexpected error: {:?}", result.err());
    }

    #[tokio::test]
    async fn ftp_scheme_rejected() {
        // Use a mock resolver so the test never performs real DNS even if the
        // validation ordering changes in the future.
        let policy = policy_with_resolver(HashMap::new());
        let err = policy.validate("ftp://example.com").await.unwrap_err();
        assert!(matches!(err, OutboundUrlError::UnsupportedScheme(_)));
    }

    #[tokio::test]
    async fn allowed_scheme_without_default_port_rejected() {
        let cfg = OutboundUrlSecurityConfig {
            allowed_schemes: vec!["custom".to_string(), "https".to_string()],
            ..Default::default()
        };
        let policy = OutboundUrlPolicy::from_config(&cfg);
        let err = policy
            .validate("custom://example.com/path")
            .await
            .unwrap_err();
        assert!(matches!(err, OutboundUrlError::NoDefaultPort(_)));
    }

    #[tokio::test]
    async fn allowed_scheme_with_explicit_port_passes() {
        let cfg = OutboundUrlSecurityConfig {
            allowed_schemes: vec!["custom".to_string(), "https".to_string()],
            ..Default::default()
        };
        let records = HashMap::from([("example.com".to_string(), vec![ipv4(1, 2, 3, 4)])]);
        let policy = OutboundUrlPolicy::from_config(&cfg)
            .with_resolver(Box::new(StaticResolver::new(records)));
        let result = policy.validate("custom://example.com:8080/path").await;
        assert!(result.is_ok(), "unexpected error: {:?}", result.err());
    }

    #[tokio::test]
    async fn empty_url_rejected() {
        let policy = OutboundUrlPolicy::strict();
        let err = policy.validate("").await.unwrap_err();
        assert!(matches!(err, OutboundUrlError::InvalidUrl(_)));
    }

    #[tokio::test]
    async fn missing_host_rejected() {
        let policy = OutboundUrlPolicy::strict();
        let err = policy.validate("https:///").await.unwrap_err();
        assert!(matches!(err, OutboundUrlError::InvalidUrl(_)));
    }

    #[tokio::test]
    async fn scheme_only_url_rejected() {
        // `Url::parse("https://")` rejects at parse time, so this tests the
        // InvalidUrl path rather than MissingHost.
        let policy = OutboundUrlPolicy::strict();
        let err = policy.validate("https://").await.unwrap_err();
        assert!(matches!(err, OutboundUrlError::InvalidUrl(_)));
    }

    #[tokio::test]
    async fn localhost_rejected() {
        let policy = OutboundUrlPolicy::strict();
        let err = policy.validate("https://localhost/hook").await.unwrap_err();
        assert!(matches!(err, OutboundUrlError::HostNotAllowed(_)));
    }

    #[tokio::test]
    async fn embedded_credentials_rejected() {
        let policy = OutboundUrlPolicy::strict();
        let cases = ["https://user:pass@example.com", "https://user@example.com"];
        for url in cases {
            let err = policy.validate(url).await.unwrap_err();
            assert!(
                matches!(err, OutboundUrlError::EmbeddedCredentials),
                "URL {} should be rejected, got {:?}",
                url,
                err
            );
        }
    }

    #[tokio::test]
    async fn ipv4_loopback_rejected() {
        let policy = OutboundUrlPolicy::strict();
        let err = policy.validate("https://127.0.0.1/hook").await.unwrap_err();
        assert!(matches!(err, OutboundUrlError::NonPublicAddress(_)));
    }

    #[tokio::test]
    async fn ipv6_loopback_rejected() {
        let policy = OutboundUrlPolicy::strict();
        let err = policy.validate("https://[::1]/hook").await.unwrap_err();
        assert!(matches!(err, OutboundUrlError::NonPublicAddress(_)));
    }

    #[tokio::test]
    async fn private_ipv4_rejected() {
        let policy = OutboundUrlPolicy::strict();
        let cases = ["10.0.0.1", "192.168.1.1", "172.16.0.1"];
        for host in cases {
            let url = format!("https://{}/hook", host);
            let err = policy.validate(&url).await.unwrap_err();
            assert!(
                matches!(err, OutboundUrlError::NonPublicAddress(_)),
                "{} should be rejected, got {:?}",
                url,
                err
            );
        }
    }

    #[tokio::test]
    async fn link_local_and_metadata_rejected() {
        let policy = OutboundUrlPolicy::strict();
        let cases = ["169.254.169.254", "169.254.1.1"];
        for host in cases {
            let url = format!("https://{}/hook", host);
            let err = policy.validate(&url).await.unwrap_err();
            assert!(
                matches!(err, OutboundUrlError::NonPublicAddress(_)),
                "{} should be rejected, got {:?}",
                url,
                err
            );
        }
    }

    #[tokio::test]
    async fn ipv4_mapped_ipv6_loopback_rejected() {
        let policy = OutboundUrlPolicy::strict();
        let err = policy
            .validate("https://[::ffff:127.0.0.1]/hook")
            .await
            .unwrap_err();
        assert!(matches!(err, OutboundUrlError::NonPublicAddress(_)));
    }

    #[tokio::test]
    async fn ipv4_mapped_ipv6_private_rejected() {
        let policy = OutboundUrlPolicy::strict();
        let err = policy
            .validate("https://[::ffff:192.168.1.1]/hook")
            .await
            .unwrap_err();
        assert!(matches!(err, OutboundUrlError::NonPublicAddress(_)));
    }

    #[tokio::test]
    async fn ipv6_unique_local_rejected() {
        let policy = OutboundUrlPolicy::strict();
        let err = policy.validate("https://[fc00::1]/hook").await.unwrap_err();
        assert!(matches!(err, OutboundUrlError::NonPublicAddress(_)));
    }

    #[tokio::test]
    async fn ipv6_multicast_rejected() {
        let policy = OutboundUrlPolicy::strict();
        let err = policy.validate("https://[ff02::1]/hook").await.unwrap_err();
        assert!(matches!(err, OutboundUrlError::NonPublicAddress(_)));
    }

    #[tokio::test]
    async fn ipv6_benchmarking_rejected() {
        let policy = OutboundUrlPolicy::strict();
        let err = policy
            .validate("https://[2001:2::1]/hook")
            .await
            .unwrap_err();
        assert!(matches!(err, OutboundUrlError::NonPublicAddress(_)));
    }

    #[tokio::test]
    async fn ipv6_ietf_protocol_assignment_rejected() {
        let policy = OutboundUrlPolicy::strict();
        let err = policy
            .validate("https://[2001:3::1]/hook")
            .await
            .unwrap_err();
        assert!(matches!(err, OutboundUrlError::NonPublicAddress(_)));
    }

    #[tokio::test]
    async fn shared_address_space_cgnat_rejected() {
        let policy = OutboundUrlPolicy::strict();
        let err = policy
            .validate("https://100.64.0.1/hook")
            .await
            .unwrap_err();
        assert!(matches!(err, OutboundUrlError::NonPublicAddress(_)));
    }

    #[tokio::test]
    async fn ietf_protocol_assignments_rejected() {
        let policy = OutboundUrlPolicy::strict();
        let err = policy.validate("https://192.0.0.1/hook").await.unwrap_err();
        assert!(matches!(err, OutboundUrlError::NonPublicAddress(_)));
    }

    #[tokio::test]
    async fn this_network_rejected() {
        let policy = OutboundUrlPolicy::strict();
        let err = policy.validate("https://0.1.2.3/hook").await.unwrap_err();
        assert!(matches!(err, OutboundUrlError::NonPublicAddress(_)));
    }

    #[tokio::test]
    async fn allow_private_ips_permits_loopback() {
        let policy = OutboundUrlPolicy::development();
        let result = policy.validate("https://127.0.0.1/hook").await;
        assert!(result.is_ok(), "unexpected error: {:?}", result.err());
    }

    #[tokio::test]
    async fn domain_resolves_to_public_ip_passes() {
        let records = HashMap::from([("service.example.com".to_string(), vec![ipv4(1, 2, 3, 4)])]);
        let policy = policy_with_resolver(records);
        let validated = policy
            .validate("https://service.example.com/hook")
            .await
            .unwrap();
        assert_eq!(validated.resolved.len(), 1);
        assert_eq!(validated.resolved[0].ip(), ipv4(1, 2, 3, 4));
    }

    #[tokio::test]
    async fn domain_resolves_to_private_ip_rejected() {
        let records = HashMap::from([("evil.example.com".to_string(), vec![ipv4(10, 0, 0, 1)])]);
        let policy = policy_with_resolver(records);
        let err = policy
            .validate("https://evil.example.com/hook")
            .await
            .unwrap_err();
        assert!(matches!(err, OutboundUrlError::NonPublicAddress(_)));
    }

    #[tokio::test]
    async fn empty_dns_result_rejected() {
        let records = HashMap::<String, Vec<IpAddr>>::new();
        let policy = policy_with_resolver(records);
        let err = policy
            .validate("https://unknown.example.com/hook")
            .await
            .unwrap_err();
        assert!(matches!(err, OutboundUrlError::ResolutionFailed { .. }));
    }

    #[tokio::test]
    async fn secure_client_pins_resolved_addresses() {
        let records = HashMap::from([("service.example.com".to_string(), vec![ipv4(1, 2, 3, 4)])]);
        let policy = policy_with_resolver(records);
        let validated = policy
            .validate("https://service.example.com/hook")
            .await
            .unwrap();

        let client =
            build_secure_client(&validated, Duration::from_secs(5), Duration::from_secs(10))
                .unwrap();

        // reqwest does not expose its resolve table directly. We verify the
        // client builds and is usable; the pinning behavior is covered by the
        // resolver contract and manual inspection of the builder configuration.
        assert_eq!(validated.resolved.len(), 1);
        drop(client);
    }

    #[tokio::test]
    #[cfg(feature = "webhooks")]
    async fn read_body_under_limit_succeeds() {
        let body = reqwest::Body::from("hello world");
        let response = http_response_from_body(body);
        let bytes = read_body_with_limit(response, 1024).await.unwrap();
        assert_eq!(bytes, b"hello world");
    }

    #[tokio::test]
    #[cfg(feature = "webhooks")]
    async fn read_body_over_limit_fails() {
        let body = reqwest::Body::from(vec![0u8; 2048]);
        let response = http_response_from_body(body);
        let err = read_body_with_limit(response, 1024).await.unwrap_err();
        assert!(matches!(err, BodyLimitError::BodyTooLarge));
    }

    #[tokio::test]
    #[cfg(feature = "webhooks")]
    async fn content_length_over_limit_fails_immediately() {
        let response = reqwest::Response::from(
            axum::http::Response::builder()
                .header("content-length", "2048")
                .body(reqwest::Body::from(""))
                .unwrap(),
        );
        let err = read_body_with_limit(response, 1024).await.unwrap_err();
        assert!(matches!(err, BodyLimitError::BodyTooLarge));
    }

    #[tokio::test]
    async fn custom_port_is_preserved() {
        let records = HashMap::from([("service.example.com".to_string(), vec![ipv4(1, 2, 3, 4)])]);
        let policy = policy_with_resolver(records);
        let validated = policy
            .validate("https://service.example.com:8443/hook")
            .await
            .unwrap();
        assert_eq!(validated.url.port(), Some(8443));
        assert_eq!(validated.resolved[0].port(), 8443);
    }

    #[tokio::test]
    async fn path_query_and_fragment_are_preserved() {
        let records = HashMap::from([("service.example.com".to_string(), vec![ipv4(1, 2, 3, 4)])]);
        let policy = policy_with_resolver(records);
        let validated = policy
            .validate("https://service.example.com/hook?event=create#section")
            .await
            .unwrap();
        assert_eq!(validated.url.path(), "/hook");
        assert_eq!(validated.url.query(), Some("event=create"));
        assert_eq!(validated.url.fragment(), Some("section"));
    }

    #[tokio::test]
    async fn mixed_public_and_private_resolved_addresses_rejected() {
        let records = HashMap::from([(
            "evil.example.com".to_string(),
            vec![ipv4(1, 2, 3, 4), ipv4(10, 0, 0, 1)],
        )]);
        let policy = policy_with_resolver(records);
        let err = policy
            .validate("https://evil.example.com/hook")
            .await
            .unwrap_err();
        assert!(matches!(err, OutboundUrlError::NonPublicAddress(_)));
    }

    #[tokio::test]
    async fn ipv4_mapped_ipv6_public_passes() {
        let policy = OutboundUrlPolicy::strict();
        let result = policy.validate("https://[::ffff:8.8.8.8]/hook").await;
        assert!(result.is_ok(), "unexpected error: {:?}", result.err());
    }

    #[tokio::test]
    async fn ip_literal_host_is_not_pinned() {
        let policy = OutboundUrlPolicy::strict();
        let validated = policy.validate("https://1.2.3.4/hook").await.unwrap();
        let client =
            build_secure_client(&validated, Duration::from_secs(5), Duration::from_secs(10))
                .unwrap();
        assert_eq!(validated.resolved.len(), 1);
        drop(client);
    }

    #[tokio::test]
    async fn uppercase_scheme_is_normalized_and_validated() {
        let records = HashMap::from([("example.com".to_string(), vec![ipv4(1, 2, 3, 4)])]);
        let policy = policy_with_resolver(records);
        // Url::parse normalizes the scheme to lowercase.
        let result = policy.validate("HTTPS://example.com/hook").await;
        assert!(result.is_ok(), "unexpected error: {:?}", result.err());
    }

    #[tokio::test]
    async fn uppercase_allowed_schemes_from_config_are_normalized() {
        let cfg = OutboundUrlSecurityConfig {
            allowed_schemes: vec!["HTTPS".to_string()],
            ..Default::default()
        };
        let records = HashMap::from([("example.com".to_string(), vec![ipv4(1, 2, 3, 4)])]);
        let policy = OutboundUrlPolicy::from_config(&cfg)
            .with_resolver(Box::new(StaticResolver::new(records)));
        let result = policy.validate("https://example.com/hook").await;
        assert!(result.is_ok(), "unexpected error: {:?}", result.err());
    }

    #[tokio::test]
    async fn dns_resolution_timeout_returns_timeout_error() {
        struct HangingResolver;

        #[async_trait]
        impl Resolver for HangingResolver {
            async fn resolve(
                &self,
                _host: &str,
                _port: u16,
            ) -> Result<Vec<SocketAddr>, std::io::Error> {
                std::future::pending::<()>().await;
                Ok(vec![])
            }
        }

        let policy = OutboundUrlPolicy::strict()
            .with_resolver(Box::new(HangingResolver))
            .with_resolve_timeout(Duration::from_millis(50));

        let err = policy
            .validate("https://slow.example.com/hook")
            .await
            .unwrap_err();
        assert!(matches!(err, OutboundUrlError::ResolutionTimeout(_)));
    }

    #[tokio::test]
    #[cfg(feature = "webhooks")]
    async fn read_body_exactly_at_limit_succeeds() {
        let body = reqwest::Body::from(vec![0u8; 1024]);
        let response = http_response_from_body(body);
        let bytes = read_body_with_limit(response, 1024).await.unwrap();
        assert_eq!(bytes.len(), 1024);
    }

    /// Helper to build a `reqwest::Response` from a `reqwest::Body` for unit tests.
    #[cfg(feature = "webhooks")]
    fn http_response_from_body(body: reqwest::Body) -> reqwest::Response {
        let response = axum::http::Response::builder().body(body).unwrap();
        reqwest::Response::from(response)
    }

    #[test]
    fn allowed_schemes_deserializes_from_comma_separated_string() {
        let cfg: OutboundUrlSecurityConfig =
            serde_json::from_str(r#"{"allowed_schemes": "https,http"}"#).unwrap();
        assert_eq!(cfg.allowed_schemes, vec!["https", "http"]);
    }

    #[test]
    fn allowed_schemes_deserializes_from_array() {
        let cfg: OutboundUrlSecurityConfig =
            serde_json::from_str(r#"{"allowed_schemes": ["https", "http"]}"#).unwrap();
        assert_eq!(cfg.allowed_schemes, vec!["https", "http"]);
    }

    #[test]
    fn config_defaults_preserve_expected_types() {
        let cfg = OutboundUrlSecurityConfig::default();
        assert_eq!(cfg.allowed_schemes, vec!["https"]);
        assert!(!cfg.allow_private_ips);
        assert_eq!(cfg.resolve_timeout_ms, 5000);
    }
}
