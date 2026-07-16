// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! Best-effort HTTP webhook log backend.
//!
//! Events are accepted through a bounded channel and dispatched from a
//! background Tokio task. `Logger::log()` only attempts to enqueue an event;
//! it never waits for queue capacity or performs network I/O.
//!
//! Each enabled endpoint has an independent outstanding-delivery budget.
//! Saturating one endpoint does not stall dispatcher admission for other
//! endpoints.

use std::{
    borrow::Cow,
    collections::HashMap,
    net::{IpAddr, SocketAddr},
    sync::Arc,
    time::Duration,
};

use anyhow::{anyhow, bail};
use async_trait::async_trait;
use bytes::Bytes;
use chrono::{DateTime, Utc};
use hmac::{Hmac, Mac};
use rand::Rng;
use reqwest::{redirect::Policy, StatusCode, Url};
use serde::Serialize;
use serde_json::Value;
use sha2::Sha256;
use tokio::{
    runtime::Handle,
    sync::{mpsc, oneshot, Semaphore, TryAcquireError},
    task::{JoinError, JoinSet},
    time::{sleep, timeout},
};
use tracing::{error, warn};
use uuid::Uuid;

use crate::config::{WebhookConfig, WebhookEndpointConfig};

use super::{LogEvent, LogLevel, Logger};

const HEADER_EVENT: &str = "X-Cube-Webhook-Event";
const HEADER_DELIVERY: &str = "X-Cube-Webhook-Delivery";
const HEADER_TIMESTAMP: &str = "X-Cube-Webhook-Timestamp";
const HEADER_SIGNATURE: &str = "X-Cube-Webhook-Signature";
const WEBHOOK_USER_AGENT: &str = "CubeAPI-Webhook/1.0";

const DEFAULT_LIFECYCLE_EVENTS: [&str; 4] = [
    "sandbox.created",
    "sandbox.deleted",
    "sandbox.paused",
    "sandbox.resumed",
];

enum Msg {
    Event(LogEvent),
    Flush(oneshot::Sender<()>),
}

#[derive(Serialize)]
struct WebhookPayload<'a> {
    id: &'a str,
    timestamp: DateTime<Utc>,
    level: LogLevel,
    event: &'a str,
    #[serde(flatten)]
    fields: &'a HashMap<String, Value>,
}

#[derive(Clone)]
struct Endpoint {
    index: usize,
    url: Url,
    events: Vec<String>,
    secret: Option<String>,
}

impl std::fmt::Debug for Endpoint {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("Endpoint")
            .field("index", &self.index)
            .field("events", &self.events)
            .field("secret_configured", &self.secret.is_some())
            .finish()
    }
}

impl Endpoint {
    fn matches(&self, event: &str) -> bool {
        events_match(&self.events, event)
    }
}

/// An enabled endpoint paired with its outstanding-delivery limiter.
///
/// The limiter lives next to the endpoint it bounds so there is no parallel
/// collection to mis-index: `Endpoint::index` is the sparse original
/// configuration index used for logging, not a position in any vector.
struct EndpointSlot {
    endpoint: Endpoint,
    outstanding: Arc<Semaphore>,
}

#[derive(Debug)]
enum DeliveryBuildError {
    /// `serde_json` serialization failed; preserve the per-endpoint drop behavior.
    Serialization(serde_json::Error),
    /// The fully serialized body exceeded the configured event-level limit.
    PayloadTooLarge { payload_bytes: usize },
}

struct Delivery {
    endpoint_index: usize,
    url: Url,
    event: String,
    id: String,
    timestamp: String,
    body: Bytes,
    signature: Option<String>,
}

impl Delivery {
    fn new(
        event: &LogEvent,
        endpoint: &Endpoint,
        fields: &HashMap<String, Value>,
        max_payload_bytes: usize,
    ) -> Result<Self, DeliveryBuildError> {
        let id = Uuid::new_v4().to_string();
        let timestamp = event.timestamp.timestamp().to_string();
        let payload = WebhookPayload {
            id: &id,
            timestamp: event.timestamp,
            level: event.level,
            event: &event.event,
            fields,
        };
        let body =
            Bytes::from(serde_json::to_vec(&payload).map_err(DeliveryBuildError::Serialization)?);
        if body.len() > max_payload_bytes {
            return Err(DeliveryBuildError::PayloadTooLarge {
                payload_bytes: body.len(),
            });
        }
        let signature = endpoint
            .secret
            .as_deref()
            .map(|secret| sign_payload(secret, &timestamp, &id, body.as_ref()));

        Ok(Self {
            endpoint_index: endpoint.index,
            url: endpoint.url.clone(),
            event: event.event.clone(),
            id,
            timestamp,
            body,
            signature,
        })
    }
}

#[derive(Clone, Copy)]
struct DeliveryOptions {
    max_retries: usize,
    max_payload_bytes: usize,
    initial_backoff_ms: u64,
    max_backoff_ms: u64,
}

/// Asynchronous best-effort HTTP webhook backend.
#[derive(Clone)]
pub struct HttpLogger {
    tx: mpsc::Sender<Msg>,
    flush_timeout: Duration,
}

impl HttpLogger {
    /// Validate the configuration, create the shared HTTP client, and start
    /// the background dispatcher.
    pub async fn new(config: WebhookConfig) -> anyhow::Result<Self> {
        validate_config(&config)?;

        let mut pinned_hosts = HashMap::new();
        let mut endpoints = Vec::new();
        for (index, endpoint_config) in config.endpoints.iter().enumerate() {
            if !endpoint_config.enabled {
                continue;
            }

            let endpoint = compile_endpoint(endpoint_config, index)?;
            if let Some(host) = domain_host(&endpoint.url).map(str::to_owned) {
                resolve_and_validate_hostname(
                    index,
                    &host,
                    endpoint_config.allow_private_urls,
                    &mut pinned_hosts,
                )
                .await?;
            }
            endpoints.push(endpoint);
        }

        let mut client_builder = reqwest::Client::builder()
            .redirect(Policy::none())
            .timeout(Duration::from_secs(config.timeout_secs))
            .connect_timeout(Duration::from_secs(WEBHOOK_CONNECT_TIMEOUT_SECS))
            .pool_idle_timeout(Duration::from_secs(WEBHOOK_POOL_IDLE_TIMEOUT_SECS))
            .pool_max_idle_per_host(WEBHOOK_POOL_MAX_IDLE_PER_HOST);
        for (host, addrs) in pinned_hosts {
            // Port zero lets reqwest use the URL's explicit port or the scheme default,
            // so one pinned hostname is safe across endpoints with different ports.
            client_builder = client_builder.resolve_to_addrs(&host, &addrs);
        }
        let client = client_builder.build()?;
        let handle = Handle::try_current()
            .map_err(|_| anyhow!("HttpLogger::new must be called from a Tokio runtime"))?;

        let queue_capacity = config.queue_capacity;
        let max_concurrency = config.max_concurrency;
        let flush_timeout = Duration::from_secs(config.flush_timeout_secs);
        let slots: Vec<EndpointSlot> =
            match per_endpoint_outstanding_budget(queue_capacity, endpoints.len()) {
                Some(outstanding_budget) => endpoints
                    .into_iter()
                    .map(|endpoint| EndpointSlot {
                        endpoint,
                        outstanding: Arc::new(Semaphore::new(outstanding_budget)),
                    })
                    .collect(),
                None => Vec::new(),
            };
        let options = DeliveryOptions {
            max_retries: config.max_retries,
            max_payload_bytes: config.max_payload_bytes,
            initial_backoff_ms: config.initial_backoff_ms,
            max_backoff_ms: config.max_backoff_ms,
        };
        let (tx, rx) = mpsc::channel(queue_capacity);

        handle.spawn(run_dispatcher(
            rx,
            Arc::new(slots),
            client,
            Arc::new(Semaphore::new(max_concurrency)),
            options,
            flush_timeout,
        ));

        Ok(Self { tx, flush_timeout })
    }
}

#[async_trait]
impl Logger for HttpLogger {
    async fn log(&self, event: LogEvent) {
        let event_name = event.event.clone();
        match self.tx.try_send(Msg::Event(event)) {
            Ok(()) => {}
            Err(mpsc::error::TrySendError::Full(_)) => {
                warn!(
                    event = %event_name,
                    "HttpLogger queue is full; dropping webhook event"
                );
            }
            Err(mpsc::error::TrySendError::Closed(_)) => {
                warn!(
                    event = %event_name,
                    "HttpLogger dispatcher is closed; dropping webhook event"
                );
            }
        }
    }

    async fn flush(&self) {
        let (reply_tx, reply_rx) = oneshot::channel();
        let completed = timeout(self.flush_timeout, async {
            if self.tx.send(Msg::Flush(reply_tx)).await.is_err() {
                return false;
            }
            reply_rx.await.is_ok()
        })
        .await;

        match completed {
            Ok(true) => {}
            Ok(false) => warn!("HttpLogger dispatcher closed before flush completed"),
            Err(_) => warn!(
                timeout_secs = self.flush_timeout.as_secs(),
                "HttpLogger flush timed out"
            ),
        }
    }

    fn name(&self) -> &'static str {
        "http"
    }
}

/// Safety ceiling for admitted unfinished deliveries across all endpoints.
///
/// Every unfinished Delivery task holds one endpoint permit, and the sum of
/// configured endpoint permits never exceeds this limit. Completed task
/// results can remain in the JoinSet until the dispatcher reaps them.
const MAX_OUTSTANDING_DELIVERY_TASKS: usize = 100_000;
/// Maximum number of retries after the initial delivery attempt.
const MAX_WEBHOOK_RETRIES: usize = 6;
/// Hard upper bound for the configurable webhook payload size limit.
const MAX_WEBHOOK_PAYLOAD_BYTES: usize = 1024 * 1024;
const WEBHOOK_CONNECT_TIMEOUT_SECS: u64 = 5;
const WEBHOOK_POOL_IDLE_TIMEOUT_SECS: u64 = 30;
/// Intentionally smaller than `max_concurrency`: `max_concurrency` bounds
/// in-flight requests, while this only limits how many idle connections are
/// retained per host after a delivery burst. Webhook traffic is bursty and
/// best-effort, so re-established connections on the next burst are an
/// accepted trade-off for a small idle pool.
const WEBHOOK_POOL_MAX_IDLE_PER_HOST: usize = 2;

/// Number of outstanding deliveries each enabled endpoint may hold.
///
/// Zero enabled endpoints need no endpoint slots. For a positive count
/// validated by `validate_enabled_endpoint_count`, integer division keeps the
/// sum of all endpoint budgets within `MAX_OUTSTANDING_DELIVERY_TASKS`.
fn per_endpoint_outstanding_budget(queue_capacity: usize, enabled_count: usize) -> Option<usize> {
    if enabled_count == 0 {
        return None;
    }

    Some(queue_capacity.min(MAX_OUTSTANDING_DELIVERY_TASKS / enabled_count))
}

fn validate_enabled_endpoint_count(enabled_count: usize) -> anyhow::Result<()> {
    if enabled_count > MAX_OUTSTANDING_DELIVERY_TASKS {
        bail!(
            "webhook configuration enables {enabled_count} endpoints; \
             at most {MAX_OUTSTANDING_DELIVERY_TASKS} may be enabled"
        );
    }
    Ok(())
}

fn validate_config(config: &WebhookConfig) -> anyhow::Result<()> {
    if config.queue_capacity == 0 {
        bail!("webhook queue_capacity must be greater than zero");
    }
    if config.timeout_secs == 0 {
        bail!("webhook timeout_secs must be greater than zero");
    }
    if config.max_concurrency == 0 {
        bail!("webhook max_concurrency must be greater than zero");
    }
    if config.flush_timeout_secs == 0 {
        bail!("webhook flush_timeout_secs must be greater than zero");
    }
    if config.max_retries > MAX_WEBHOOK_RETRIES {
        bail!("webhook max_retries must not exceed {MAX_WEBHOOK_RETRIES}");
    }
    if config.max_payload_bytes == 0 {
        bail!("webhook max_payload_bytes must be greater than zero");
    }
    if config.max_payload_bytes > MAX_WEBHOOK_PAYLOAD_BYTES {
        bail!("webhook max_payload_bytes must not exceed {MAX_WEBHOOK_PAYLOAD_BYTES}");
    }
    if config.initial_backoff_ms == 0 {
        bail!("webhook initial_backoff_ms must be greater than zero");
    }
    if config.max_backoff_ms == 0 {
        bail!("webhook max_backoff_ms must be greater than zero");
    }
    if config.initial_backoff_ms > config.max_backoff_ms {
        bail!("webhook initial_backoff_ms must not exceed max_backoff_ms");
    }
    let enabled_endpoints = config
        .endpoints
        .iter()
        .filter(|endpoint| endpoint.enabled)
        .count();
    validate_enabled_endpoint_count(enabled_endpoints)?;
    Ok(())
}

fn compile_endpoint(config: &WebhookEndpointConfig, index: usize) -> anyhow::Result<Endpoint> {
    if config.secret.as_deref() == Some("") {
        bail!("webhook endpoint {index} has an empty secret");
    }
    if config.events.iter().any(String::is_empty) {
        bail!("webhook endpoint {index} has an empty event name");
    }
    if config.events.iter().any(|event| event == "*") && config.events.len() != 1 {
        bail!("webhook endpoint {index} mixes '*' with explicit event names");
    }

    let url = Url::parse(&config.url)
        .map_err(|_| anyhow!("webhook endpoint {index} has an invalid URL"))?;
    if !matches!(url.scheme(), "http" | "https") || url.host_str().is_none() {
        bail!("webhook endpoint {index} must use an absolute HTTP(S) URL");
    }
    if !url.username().is_empty() || url.password().is_some() {
        bail!("webhook endpoint {index} must not embed credentials in the URL");
    }
    match classify_host(&url) {
        HostClass::Public => {}
        HostClass::NonPublic if config.allow_private_urls => {}
        HostClass::NonPublic => bail!(
            "webhook endpoint {index} targets a private, loopback, or link-local address; \
             set allow_private_urls=true on this endpoint to permit it"
        ),
        HostClass::Invalid => bail!(
            "webhook endpoint {index} targets an unspecified, broadcast, or multicast address"
        ),
    }

    Ok(Endpoint {
        index,
        url,
        events: config.events.clone(),
        secret: config.secret.clone(),
    })
}

/// Classification shared by literal URL hosts and startup-resolved addresses.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
enum HostClass {
    /// Globally routable target; always accepted.
    Public,
    /// Loopback, private, link-local, unique-local, or `localhost`; accepted
    /// only when the endpoint sets `allow_private_urls`.
    NonPublic,
    /// Unspecified, broadcast, or multicast; never a valid webhook receiver.
    Invalid,
}

fn classify_host(url: &Url) -> HostClass {
    let Some(host) = url.host_str() else {
        return HostClass::Invalid;
    };
    if host.eq_ignore_ascii_case("localhost") {
        return HostClass::NonPublic;
    }
    let ip_literal = host
        .strip_prefix('[')
        .and_then(|inner| inner.strip_suffix(']'))
        .unwrap_or(host);
    match ip_literal.parse::<IpAddr>() {
        Ok(ip) => classify_ip(ip),
        Err(_) => HostClass::Public,
    }
}

fn domain_host(url: &Url) -> Option<&str> {
    let host = url.host_str()?;
    let ip_literal = host
        .strip_prefix('[')
        .and_then(|inner| inner.strip_suffix(']'))
        .unwrap_or(host);
    ip_literal.parse::<IpAddr>().is_err().then_some(host)
}

/// Resolves a webhook hostname during startup and validates the addresses
/// before pinning them into the shared reqwest client.
async fn resolve_and_validate_hostname(
    endpoint_index: usize,
    host: &str,
    allow_private_urls: bool,
    pinned_hosts: &mut HashMap<String, Vec<SocketAddr>>,
) -> anyhow::Result<()> {
    if let Some(addrs) = pinned_hosts.get(host) {
        let ips = addrs.iter().map(|address| address.ip()).collect::<Vec<_>>();
        return validate_resolved_ips(endpoint_index, &ips, allow_private_urls);
    }

    let addresses = tokio::net::lookup_host((host, 0))
        .await
        .map_err(|_| anyhow!("webhook endpoint {endpoint_index} hostname resolution failed"))?;
    let mut pinned_addrs = Vec::new();
    for mut address in addresses {
        address.set_port(0);
        if !pinned_addrs.contains(&address) {
            pinned_addrs.push(address);
        }
    }

    let ips = pinned_addrs
        .iter()
        .map(|address| address.ip())
        .collect::<Vec<_>>();
    validate_resolved_ips(endpoint_index, &ips, allow_private_urls)?;
    pinned_hosts.insert(host.to_string(), pinned_addrs);
    Ok(())
}

fn validate_resolved_ips(
    endpoint_index: usize,
    ips: &[IpAddr],
    allow_private_urls: bool,
) -> anyhow::Result<()> {
    if ips.is_empty() {
        bail!("webhook endpoint {endpoint_index} hostname resolved to no addresses");
    }

    for &ip in ips {
        match classify_ip(ip) {
            HostClass::Public => {}
            HostClass::NonPublic if allow_private_urls => {}
            HostClass::NonPublic => bail!(
                "webhook endpoint {endpoint_index} hostname resolves to a private, loopback, unique-local, or \
                 link-local address; set allow_private_urls=true on this endpoint to permit it"
            ),
            HostClass::Invalid => bail!(
                "webhook endpoint {endpoint_index} hostname resolves to an unspecified, \
                 broadcast, or multicast address"
            ),
        }
    }

    Ok(())
}

fn classify_ip(ip: IpAddr) -> HostClass {
    match ip {
        IpAddr::V4(ip) => {
            if ip.is_unspecified() || ip.is_broadcast() || ip.is_multicast() {
                HostClass::Invalid
            } else if ip.is_loopback() || ip.is_private() || ip.is_link_local() {
                HostClass::NonPublic
            } else {
                HostClass::Public
            }
        }
        IpAddr::V6(ip) => {
            if let Some(mapped) = ip.to_ipv4_mapped() {
                return classify_ip(IpAddr::V4(mapped));
            }
            if ip.is_unspecified() || ip.is_multicast() {
                HostClass::Invalid
            } else if ip.is_loopback() || ip.is_unique_local() || ip.is_unicast_link_local() {
                HostClass::NonPublic
            } else {
                HostClass::Public
            }
        }
    }
}

async fn run_dispatcher(
    mut rx: mpsc::Receiver<Msg>,
    endpoints: Arc<Vec<EndpointSlot>>,
    client: reqwest::Client,
    semaphore: Arc<Semaphore>,
    options: DeliveryOptions,
    flush_timeout: Duration,
) {
    let mut deliveries = JoinSet::new();

    loop {
        tokio::select! {
            biased;

            result = deliveries.join_next(), if !deliveries.is_empty() => {
                if let Some(result) = result {
                    log_delivery_task_result(result);
                }
            }
            message = rx.recv() => {
                match message {
                    Some(Msg::Event(event)) => {
                        spawn_deliveries(
                            &mut deliveries,
                            event,
                            &endpoints,
                            &client,
                            &semaphore,
                            options,
                        );
                    }
                    Some(Msg::Flush(reply)) => {
                        flush_deliveries(&mut deliveries, flush_timeout).await;
                        let _ = reply.send(());
                    }
                    None => {
                        flush_deliveries(&mut deliveries, flush_timeout).await;
                        break;
                    }
                }
            }
        }
    }
}

fn spawn_deliveries(
    deliveries: &mut JoinSet<()>,
    event: LogEvent,
    endpoints: &[EndpointSlot],
    client: &reqwest::Client,
    semaphore: &Arc<Semaphore>,
    options: DeliveryOptions,
) {
    let matching: Vec<&EndpointSlot> = endpoints
        .iter()
        .filter(|slot| slot.endpoint.matches(&event.event))
        .collect();

    if matching.is_empty() {
        return;
    }

    let fields = sanitized_fields(&event.fields);
    let mut built = Vec::with_capacity(matching.len());

    for slot in &matching {
        match Delivery::new(&event, &slot.endpoint, &fields, options.max_payload_bytes) {
            Ok(delivery) => built.push((*slot, delivery)),
            Err(DeliveryBuildError::PayloadTooLarge { payload_bytes }) => {
                warn!(
                    event = %event.event,
                    payload_bytes,
                    limit_bytes = options.max_payload_bytes,
                    matched_endpoints = matching.len(),
                    drop_reason = "payload_too_large",
                    "webhook payload exceeds max_payload_bytes; dropping event"
                );
                return;
            }
            Err(DeliveryBuildError::Serialization(_serialization_error)) => error!(
                endpoint_index = slot.endpoint.index,
                event = %event.event,
                "failed to build webhook delivery; dropping delivery"
            ),
        }
    }

    // Every delivery is built before any permit is acquired, so an oversized
    // event is rejected for all matching endpoints without consuming budget.
    for (slot, delivery) in built {
        // Admission is per endpoint and never awaits: waiting for a saturated
        // endpoint here would stall the dispatcher and with it every other
        // endpoint's traffic. A full endpoint sheds only its own delivery.
        match slot.outstanding.clone().try_acquire_owned() {
            Ok(permit) => {
                let delivery_task =
                    deliver_with_retry(delivery, client.clone(), semaphore.clone(), options);
                deliveries.spawn(async move {
                    // The permit counts unfinished deliveries, so it is held
                    // through retries and backoff and released only when this
                    // task ends - by completion, cancellation, or panic.
                    let _permit = permit;
                    delivery_task.await;
                });
            }
            Err(TryAcquireError::NoPermits) => warn!(
                endpoint_index = slot.endpoint.index,
                event = %delivery.event,
                drop_reason = "endpoint_backlog_full",
                "webhook endpoint has too many outstanding deliveries; dropping delivery"
            ),
            Err(TryAcquireError::Closed) => warn!(
                endpoint_index = slot.endpoint.index,
                event = %delivery.event,
                drop_reason = "endpoint_backlog_closed",
                "webhook endpoint delivery limiter is closed; dropping delivery"
            ),
        }
    }
}

async fn flush_deliveries(deliveries: &mut JoinSet<()>, flush_timeout: Duration) {
    let drain = async {
        while let Some(result) = deliveries.join_next().await {
            log_delivery_task_result(result);
        }
    };

    if timeout(flush_timeout, drain).await.is_err() {
        let pending = deliveries.len();
        warn!(
            pending,
            timeout_secs = flush_timeout.as_secs(),
            "HttpLogger delivery flush timed out; aborting pending deliveries"
        );
        deliveries.abort_all();
        while let Some(result) = deliveries.join_next().await {
            log_delivery_task_result(result);
        }
    }
}

fn log_delivery_task_result(result: Result<(), JoinError>) {
    if let Err(join_error) = result {
        if join_error.is_cancelled() {
            // Expected during graceful shutdown when a flush timeout aborts
            // pending deliveries; not a delivery-task defect.
            warn!("HttpLogger delivery task cancelled");
        } else {
            error!(
                panicked = join_error.is_panic(),
                "HttpLogger delivery task failed"
            );
        }
    }
}

async fn deliver_with_retry(
    delivery: Delivery,
    client: reqwest::Client,
    semaphore: Arc<Semaphore>,
    options: DeliveryOptions,
) {
    for attempt in 0..=options.max_retries {
        // The permit is intentionally acquired per attempt and released before
        // the backoff sleep, so `max_concurrency` bounds in-flight HTTP
        // requests rather than deliveries idling between retries.
        let permit = match semaphore.acquire().await {
            Ok(permit) => permit,
            Err(_) => {
                error!(
                    endpoint_index = delivery.endpoint_index,
                    event = %delivery.event,
                    delivery_id = %delivery.id,
                    "HttpLogger concurrency limiter closed; dropping delivery"
                );
                return;
            }
        };
        let result = send_once(&client, &delivery).await;
        drop(permit);

        match result {
            Ok(status) if status.is_success() => return,
            Ok(status) if is_retryable_status(status) && attempt < options.max_retries => {
                warn!(
                    endpoint_index = delivery.endpoint_index,
                    event = %delivery.event,
                    delivery_id = %delivery.id,
                    status = status.as_u16(),
                    attempt = attempt + 1,
                    "webhook delivery failed; retrying"
                );
            }
            Ok(status) if is_retryable_status(status) => {
                error!(
                    endpoint_index = delivery.endpoint_index,
                    event = %delivery.event,
                    delivery_id = %delivery.id,
                    status = status.as_u16(),
                    attempts = attempt + 1,
                    "webhook delivery retries exhausted"
                );
                return;
            }
            Ok(status) => {
                warn!(
                    endpoint_index = delivery.endpoint_index,
                    event = %delivery.event,
                    delivery_id = %delivery.id,
                    status = status.as_u16(),
                    attempt = attempt + 1,
                    "webhook delivery rejected without retry"
                );
                return;
            }
            Err(request_error) if attempt < options.max_retries => {
                warn!(
                    endpoint_index = delivery.endpoint_index,
                    event = %delivery.event,
                    delivery_id = %delivery.id,
                    error_kind = request_error_kind(&request_error),
                    attempt = attempt + 1,
                    "webhook delivery failed; retrying"
                );
            }
            Err(request_error) => {
                error!(
                    endpoint_index = delivery.endpoint_index,
                    event = %delivery.event,
                    delivery_id = %delivery.id,
                    error_kind = request_error_kind(&request_error),
                    attempts = attempt + 1,
                    "webhook delivery retries exhausted"
                );
                return;
            }
        }

        sleep(backoff_delay(
            attempt,
            options.initial_backoff_ms,
            options.max_backoff_ms,
        ))
        .await;
    }
}

async fn send_once(
    client: &reqwest::Client,
    delivery: &Delivery,
) -> Result<StatusCode, reqwest::Error> {
    let mut request = client
        .post(delivery.url.clone())
        .header(reqwest::header::CONTENT_TYPE, "application/json")
        .header(reqwest::header::USER_AGENT, WEBHOOK_USER_AGENT)
        .header(HEADER_EVENT, &delivery.event)
        .header(HEADER_DELIVERY, &delivery.id)
        .header(HEADER_TIMESTAMP, &delivery.timestamp)
        .body(delivery.body.clone());

    if let Some(signature) = &delivery.signature {
        request = request.header(HEADER_SIGNATURE, signature);
    }

    request.send().await.map(|response| response.status())
}

#[cfg(test)]
fn endpoint_matches_event(endpoint: &WebhookEndpointConfig, event: &str) -> bool {
    endpoint.enabled && events_match(&endpoint.events, event)
}

fn events_match(events: &[String], event: &str) -> bool {
    if events.is_empty() {
        return is_default_lifecycle_event(event);
    }
    events
        .iter()
        .any(|configured| configured == "*" || configured == event)
}

fn is_default_lifecycle_event(event: &str) -> bool {
    DEFAULT_LIFECYCLE_EVENTS.contains(&event)
}

fn sign_payload(secret: &str, timestamp: &str, delivery_id: &str, body: &[u8]) -> String {
    let mut mac = Hmac::<Sha256>::new_from_slice(secret.as_bytes())
        .expect("HMAC-SHA256 accepts keys of any length");
    mac.update(timestamp.as_bytes());
    mac.update(b".");
    mac.update(delivery_id.as_bytes());
    mac.update(b".");
    mac.update(body);
    format!("v1={}", hex::encode(mac.finalize().into_bytes()))
}

fn is_retryable_status(status: StatusCode) -> bool {
    status == StatusCode::REQUEST_TIMEOUT
        || status.as_u16() == 425
        || status == StatusCode::TOO_MANY_REQUESTS
        || status.is_server_error()
}

fn backoff_delay(attempt: usize, initial_ms: u64, max_ms: u64) -> Duration {
    if initial_ms == 0 || max_ms == 0 {
        return Duration::from_millis(0);
    }

    let multiplier = 1_u64
        .checked_shl(attempt.min(63) as u32)
        .unwrap_or(u64::MAX);
    let base_ms = initial_ms.saturating_mul(multiplier).min(max_ms);

    if base_ms <= 1 {
        return Duration::from_millis(base_ms);
    }

    let jitter_ms = rand::thread_rng().gen_range(0..base_ms);
    let delay_ms = (base_ms / 2).saturating_add(jitter_ms).min(max_ms);

    Duration::from_millis(delay_ms)
}

fn request_error_kind(error: &reqwest::Error) -> &'static str {
    if error.is_timeout() {
        "timeout"
    } else if error.is_connect() {
        "connect"
    } else if error.is_request() {
        "request"
    } else if error.is_body() {
        "body"
    } else {
        "unknown"
    }
}

const MAX_SANITIZE_DEPTH: usize = 64;
const SANITIZE_DEPTH_TRUNCATION_PLACEHOLDER: &str = "[truncated: maximum nesting depth]";

fn sanitized_fields(fields: &HashMap<String, Value>) -> HashMap<String, Value> {
    fields
        .iter()
        .filter(|(key, _)| !is_reserved_field(key) && !is_sensitive_field(key))
        .map(|(key, value)| (key.clone(), sanitize_value(value)))
        .collect()
}

fn sanitize_value(value: &Value) -> Value {
    sanitize_value_with_depth(value, 0)
}

fn sanitize_value_with_depth(value: &Value, depth: usize) -> Value {
    match value {
        Value::Object(_) | Value::Array(_) if depth >= MAX_SANITIZE_DEPTH => {
            Value::String(SANITIZE_DEPTH_TRUNCATION_PLACEHOLDER.to_string())
        }
        Value::Object(values) => Value::Object(
            values
                .iter()
                .filter(|(key, _)| !is_sensitive_field(key))
                .map(|(key, value)| (key.clone(), sanitize_value_with_depth(value, depth + 1)))
                .collect(),
        ),
        Value::Array(values) => Value::Array(
            values
                .iter()
                .map(|value| sanitize_value_with_depth(value, depth + 1))
                .collect(),
        ),
        _ => value.clone(),
    }
}

fn is_reserved_field(key: &str) -> bool {
    matches!(key, "id" | "timestamp" | "level" | "event")
}

/// Returns true for field names that are likely to carry credentials or signing material.
///
/// The matcher intentionally avoids unrestricted substring checks. This keeps benign
/// metadata such as `token_bucket_config`, `password_reset_url`, or
/// `credentials_verification_status` from being stripped while still redacting common
/// credential field names and compound names such as `access_token`, `private_key`,
/// `db_password`, or `webhook_signature`.
fn is_sensitive_field(key: &str) -> bool {
    let normalized = normalize_field_name(key);
    let normalized: &str = normalized.as_ref();

    if matches_sensitive_name(normalized) {
        return true;
    }

    const DERIVED_MATERIAL_SUFFIXES: &[&str] = &[
        "_hash",
        "_digest",
        "_pem",
        "_der",
        "_hex",
        "_b64",
        "_base64",
        "_encrypted",
    ];

    let mut base_name = normalized;
    while let Some(stripped) = DERIVED_MATERIAL_SUFFIXES
        .iter()
        .find_map(|suffix| base_name.strip_suffix(suffix))
    {
        base_name = stripped;
        if matches_sensitive_name(base_name) {
            return true;
        }
    }

    false
}

fn matches_sensitive_name(normalized: &str) -> bool {
    if normalized.is_empty() {
        return false;
    }

    const EXACT_FIELDS: &[&str] = &[
        "api_key",
        "apikey",
        "api_token",
        "auth",
        "auth_header",
        "authorization",
        "authorization_header",
        "bearer",
        "bearer_token",
        "client_secret",
        "cookie",
        "credential",
        "credentials",
        "csrf",
        "csrf_token",
        "hashed_password",
        "id_token",
        "idtoken",
        "jwt",
        "jwt_token",
        "passphrase",
        "password",
        "password_digest",
        "password_hash",
        "passwd",
        "private_key",
        "private_key_pem",
        "privatekey",
        "refresh_token",
        "refreshtoken",
        "secret",
        "secret_key",
        "secret_value",
        "secretkey",
        "session_token",
        "set_cookie",
        "signature",
        "token",
        "token_value",
        "webhook_secret",
        "webhook_signature",
        "x_api_key",
        "xapikey",
    ];

    if EXACT_FIELDS.contains(&normalized) {
        return true;
    }

    const SENSITIVE_SUFFIXES: &[&str] = &[
        "_api_key",
        "_auth",
        "_authorization",
        "_bearer",
        "_cookie",
        "_credential",
        "_credentials",
        "_csrf",
        "_jwt",
        "_passphrase",
        "_password",
        "_passwd",
        "_private_key",
        "_secret",
        "_secret_key",
        "_signature",
        "_token",
    ];

    if SENSITIVE_SUFFIXES
        .iter()
        .any(|suffix| normalized.ends_with(suffix))
    {
        return true;
    }

    const SENSITIVE_PREFIXES: &[&str] = &[
        "access_token_",
        "api_key_",
        "authorization_",
        "bearer_",
        "client_secret_",
        "csrf_",
        "id_token_",
        "jwt_",
        "private_key_",
        "refresh_token_",
        "secret_key_",
        "set_cookie_",
        "x_api_key_",
    ];

    SENSITIVE_PREFIXES
        .iter()
        .any(|prefix| normalized.starts_with(prefix))
}

fn normalize_field_name(key: &str) -> Cow<'_, str> {
    let trimmed = key.trim_matches('_');
    if !trimmed
        .bytes()
        .any(|byte| byte.is_ascii_uppercase() || matches!(byte, b'-' | b'.' | b' '))
    {
        return Cow::Borrowed(trimmed);
    }

    let mut normalized = String::with_capacity(trimmed.len());
    let mut chars = trimmed.chars().peekable();
    let mut previous = None;

    while let Some(ch) = chars.next() {
        if matches!(ch, '_' | '-' | '.' | ' ') {
            if !normalized.is_empty() {
                normalized.push('_');
            }
            previous = Some(ch);
            continue;
        }

        if ch.is_ascii_uppercase() {
            let follows_lowercase_or_digit = previous
                .is_some_and(|previous| previous.is_ascii_lowercase() || previous.is_ascii_digit());
            let ends_acronym = previous.is_some_and(|previous| previous.is_ascii_uppercase())
                && chars.peek().is_some_and(|next| next.is_ascii_lowercase());

            if !normalized.is_empty()
                && !normalized.ends_with('_')
                && (follows_lowercase_or_digit || ends_acronym)
            {
                normalized.push('_');
            }
        }

        normalized.push(ch.to_ascii_lowercase());
        previous = Some(ch);
    }

    while normalized.ends_with('_') {
        normalized.pop();
    }

    Cow::Owned(normalized)
}

#[cfg(test)]
mod tests {
    use std::{
        collections::VecDeque,
        sync::{
            atomic::{AtomicUsize, Ordering},
            Arc, Mutex,
        },
    };

    use axum::{
        extract::State,
        http::HeaderMap,
        routing::{get, post},
        Router,
    };

    use super::*;

    fn valid_webhook_config() -> WebhookConfig {
        WebhookConfig {
            endpoints: vec![endpoint(&["*"])],
            queue_capacity: 16,
            max_payload_bytes: 1024 * 1024,
            timeout_secs: 5,
            max_retries: 2,
            initial_backoff_ms: 100,
            max_backoff_ms: 1_000,
            max_concurrency: 4,
            flush_timeout_secs: 5,
        }
    }

    #[test]
    fn validate_config_accepts_valid_values() {
        let config = valid_webhook_config();
        assert!(validate_config(&config).is_ok());
    }

    #[test]
    fn validate_config_rejects_invalid_values() {
        let mut config = valid_webhook_config();
        config.queue_capacity = 0;
        assert!(validate_config(&config).is_err());

        let mut config = valid_webhook_config();
        config.timeout_secs = 0;
        assert!(validate_config(&config).is_err());

        let mut config = valid_webhook_config();
        config.max_concurrency = 0;
        assert!(validate_config(&config).is_err());

        let mut config = valid_webhook_config();
        config.flush_timeout_secs = 0;
        assert!(validate_config(&config).is_err());

        let mut config = valid_webhook_config();
        config.initial_backoff_ms = 0;
        let error = validate_config(&config).unwrap_err().to_string();
        assert!(error.contains("initial_backoff_ms must be greater than zero"));

        let mut config = valid_webhook_config();
        config.max_backoff_ms = 0;
        let error = validate_config(&config).unwrap_err().to_string();
        assert!(error.contains("max_backoff_ms must be greater than zero"));

        let mut config = valid_webhook_config();
        config.initial_backoff_ms = 2_000;
        config.max_backoff_ms = 1_000;
        assert!(validate_config(&config).is_err());
    }

    #[test]
    fn validate_config_accepts_max_retries_at_upper_bound() {
        let mut config = valid_webhook_config();
        config.max_retries = MAX_WEBHOOK_RETRIES;
        assert!(validate_config(&config).is_ok());
    }

    #[test]
    fn validate_config_rejects_max_retries_above_upper_bound_without_leaking_config() {
        let mut config = valid_webhook_config();
        config.max_retries = MAX_WEBHOOK_RETRIES + 1;
        config.endpoints[0].url = concat!(
            "https://embedded-user:embedded-password@sensitive.example.test/",
            "hook?token=query-token"
        )
        .to_string();
        config.endpoints[0].secret = Some("endpoint-signing-secret".to_string());

        let error = validate_config(&config).unwrap_err().to_string();

        assert!(error.contains("max_retries"));
        assert!(error.contains(&MAX_WEBHOOK_RETRIES.to_string()));
        for leaked in [
            "sensitive.example.test",
            "embedded-user",
            "embedded-password",
            "query-token",
            "endpoint-signing-secret",
        ] {
            assert!(!error.contains(leaked), "error must not contain {leaked}");
        }
    }

    #[test]
    fn validate_config_rejects_zero_and_oversized_max_payload_bytes() {
        let mut config = valid_webhook_config();
        config.max_payload_bytes = 0;
        let error = validate_config(&config).unwrap_err().to_string();
        assert!(error.contains("max_payload_bytes must be greater than zero"));

        let mut config = valid_webhook_config();
        config.max_payload_bytes = MAX_WEBHOOK_PAYLOAD_BYTES;
        assert!(validate_config(&config).is_ok());

        let mut config = valid_webhook_config();
        config.max_payload_bytes = MAX_WEBHOOK_PAYLOAD_BYTES + 1;
        config.endpoints[0].url = concat!(
            "https://embedded-user:embedded-password@sensitive.example.test/",
            "hook?token=query-token"
        )
        .to_string();
        config.endpoints[0].secret = Some("endpoint-signing-secret".to_string());

        let error = validate_config(&config).unwrap_err().to_string();

        assert!(error.contains("max_payload_bytes"));
        assert!(error.contains(&MAX_WEBHOOK_PAYLOAD_BYTES.to_string()));
        for leaked in [
            "sensitive.example.test",
            "embedded-user",
            "embedded-password",
            "query-token",
            "endpoint-signing-secret",
        ] {
            assert!(!error.contains(leaked), "error must not contain {leaked}");
        }
    }

    #[test]
    fn backoff_delay_handles_zero_and_single_millisecond_values() {
        assert_eq!(backoff_delay(0, 0, 1_000), Duration::from_millis(0));
        assert_eq!(backoff_delay(0, 100, 0), Duration::from_millis(0));
        assert_eq!(backoff_delay(0, 1, 1_000), Duration::from_millis(1));
    }

    #[test]
    fn backoff_delay_applies_exponential_base_with_jitter_bounds() {
        for attempt in 0..10 {
            let delay = backoff_delay(attempt, 100, 1_000);
            let millis = delay.as_millis() as u64;

            let multiplier = 1_u64 << attempt;
            let base_ms = 100_u64.saturating_mul(multiplier).min(1_000);
            let lower_bound = base_ms / 2;
            let upper_bound = lower_bound.saturating_add(base_ms - 1).min(1_000);

            assert!(
                (lower_bound..=upper_bound).contains(&millis),
                "attempt {attempt}: delay {millis}ms outside expected jitter range \
                 [{lower_bound}, {upper_bound}]"
            );
        }
    }

    #[test]
    fn backoff_delay_caps_extreme_values() {
        for attempt in [10, 63, 64, usize::MAX] {
            let delay = backoff_delay(attempt, 1_000, 1_500);
            let millis = delay.as_millis() as u64;

            assert!(
                (750..=1_500).contains(&millis),
                "attempt {attempt}: capped delay {millis}ms outside expected range"
            );
        }
    }

    #[test]
    fn per_endpoint_budget_preserves_queue_scale() {
        assert_eq!(per_endpoint_outstanding_budget(1024, 1), Some(1024));
        assert_eq!(per_endpoint_outstanding_budget(8, 3), Some(8));
        assert_eq!(
            per_endpoint_outstanding_budget(usize::MAX, MAX_OUTSTANDING_DELIVERY_TASKS),
            Some(1)
        );
        assert_eq!(per_endpoint_outstanding_budget(1024, 0), None);
    }

    #[test]
    fn per_endpoint_budget_holds_aggregate_bound_for_all_valid_counts() {
        for enabled_count in 1..=MAX_OUTSTANDING_DELIVERY_TASKS {
            for queue_capacity in [1, 1024, usize::MAX] {
                let budget = per_endpoint_outstanding_budget(queue_capacity, enabled_count)
                    .expect("positive endpoint count must have a budget");
                assert!(budget >= 1, "budget must stay positive");
                assert!(budget <= queue_capacity);
                let aggregate = enabled_count
                    .checked_mul(budget)
                    .expect("validated endpoint budget must not overflow");
                assert!(
                    aggregate <= MAX_OUTSTANDING_DELIVERY_TASKS,
                    "aggregate bound violated for {enabled_count} endpoints"
                );
            }
        }
    }

    #[test]
    fn validate_enabled_endpoint_count_uses_outstanding_delivery_limit() {
        assert!(validate_enabled_endpoint_count(MAX_OUTSTANDING_DELIVERY_TASKS).is_ok());

        let rejected_count = MAX_OUTSTANDING_DELIVERY_TASKS + 1;
        let error = validate_enabled_endpoint_count(rejected_count)
            .unwrap_err()
            .to_string();
        assert_eq!(
            error,
            format!(
                "webhook configuration enables {rejected_count} endpoints; \
                 at most {MAX_OUTSTANDING_DELIVERY_TASKS} may be enabled"
            )
        );
    }

    #[test]
    fn sensitive_field_matching_redacts_common_credentials() {
        let sensitive_fields = [
            "password",
            "db_password",
            "dbPassword",
            "passwd",
            "client_secret",
            "clientSecret",
            "access_token",
            "accessToken",
            "refresh-token",
            "refreshToken",
            "id.token",
            "IDToken",
            "private_key",
            "privateKey",
            "private_key_pem",
            "api_key",
            "APIKey",
            "APISecret",
            "x-api-key",
            "jwt",
            "bearer",
            "auth",
            "authToken",
            "csrf",
            "authorization",
            "set-cookie",
            "webhook_signature",
            "webhookSignature",
            "gitlabAccessToken",
        ];

        for field in sensitive_fields {
            assert!(is_sensitive_field(field), "{field} should be redacted");
        }
    }

    #[test]
    fn sensitive_field_matching_redacts_derived_secret_material() {
        for field in [
            "db_password_hash",
            "dbPasswordHash",
            "user_password_digest",
            "api_key_hash",
            "client_secret_b64",
            "webhook_signature_hash",
            "password_hash_b64",
            "dbPasswordHashB64",
        ] {
            assert!(is_sensitive_field(field), "{field} should be redacted");
        }
    }

    #[test]
    fn sensitive_field_matching_keeps_non_secret_metadata() {
        let safe_fields = [
            "token_bucket_config",
            "tokenBucketConfig",
            "password_reset_url",
            "passwordResetUrl",
            "credentials_verification_status",
            "credentialsVerificationStatus",
            "signature_algorithm",
            "signatureAlgorithm",
            "secretary_name",
            "secretaryName",
            "authored_by",
            "authoredBy",
            "cookie_policy",
            "cookiePolicy",
            "auth_token_status",
            "authTokenStatus",
            "token_refresh_interval",
            "tokenRefreshInterval",
            "password_expiry_days",
            "passwordExpiryDays",
            "commit_hash",
            "commitHash",
            "content_hash",
            "contentHash",
            "file_digest",
            "fileDigest",
        ];

        for field in safe_fields {
            assert!(!is_sensitive_field(field), "{field} should not be redacted");
        }
    }

    #[test]
    fn field_name_normalization_handles_case_boundaries_and_acronyms() {
        assert!(matches!(
            normalize_field_name("access_token"),
            Cow::Borrowed(_)
        ));
        assert!(matches!(normalize_field_name("accessToken"), Cow::Owned(_)));

        for (field, expected) in [
            ("access_token", "access_token"),
            ("accessToken", "access_token"),
            ("authToken", "auth_token"),
            ("ClientSecret", "client_secret"),
            ("refreshToken", "refresh_token"),
            ("privateKey", "private_key"),
            ("webhookSignature", "webhook_signature"),
            ("dbPassword", "db_password"),
            ("APIKey", "api_key"),
            ("APISecret", "api_secret"),
            ("IDToken", "id_token"),
            ("gitlabAccessToken", "gitlab_access_token"),
            ("refresh-token", "refresh_token"),
            ("private.key", "private_key"),
        ] {
            assert_eq!(normalize_field_name(field).as_ref(), expected);
        }
    }

    fn endpoint(events: &[&str]) -> WebhookEndpointConfig {
        WebhookEndpointConfig {
            url: "http://127.0.0.1:1/webhook".to_string(),
            events: events.iter().map(|event| (*event).to_string()).collect(),
            secret: None,
            enabled: true,
            allow_private_urls: true,
        }
    }

    fn test_config(url: String, queue_capacity: usize, max_retries: usize) -> WebhookConfig {
        WebhookConfig {
            endpoints: vec![WebhookEndpointConfig {
                url,
                events: vec!["*".to_string()],
                secret: None,
                enabled: true,
                allow_private_urls: true,
            }],
            queue_capacity,
            max_payload_bytes: 1024 * 1024,
            timeout_secs: 1,
            max_retries,
            initial_backoff_ms: 1,
            max_backoff_ms: 2,
            max_concurrency: 4,
            flush_timeout_secs: 2,
        }
    }

    fn test_event() -> LogEvent {
        LogEvent::new(LogLevel::Info, "sandbox.created")
            .field("sandbox_id", "sandbox-123")
            .field("template_id", "template-456")
    }

    #[test]
    fn event_filtering_obeys_endpoint_configuration() {
        let mut disabled = endpoint(&["*"]);
        disabled.enabled = false;
        assert!(!endpoint_matches_event(&disabled, "sandbox.created"));

        let defaults = endpoint(&[]);
        for event in DEFAULT_LIFECYCLE_EVENTS {
            assert!(endpoint_matches_event(&defaults, event));
        }
        assert!(!endpoint_matches_event(&defaults, "api.request"));

        assert!(endpoint_matches_event(&endpoint(&["*"]), "api.request"));

        let created_only = endpoint(&["sandbox.created"]);
        assert!(endpoint_matches_event(&created_only, "sandbox.created"));
        assert!(!endpoint_matches_event(&created_only, "sandbox.deleted"));

        let mixed_wildcard = endpoint(&["*", "sandbox.created"]);
        assert!(compile_endpoint(&mixed_wildcard, 0).is_err());
    }

    fn endpoint_with_url(url: &str, allow_private_urls: bool) -> WebhookEndpointConfig {
        let mut config = endpoint(&["*"]);
        config.url = url.to_string();
        config.allow_private_urls = allow_private_urls;
        config
    }

    fn compile_endpoint_error(config: &WebhookEndpointConfig) -> String {
        match compile_endpoint(config, 0) {
            Ok(_) => panic!("endpoint should be rejected"),
            Err(err) => err.to_string(),
        }
    }

    fn parsed_ips(addresses: &[&str]) -> Vec<IpAddr> {
        addresses
            .iter()
            .map(|address| address.parse().unwrap())
            .collect()
    }

    #[test]
    fn resolved_ip_validation_accepts_all_public_addresses() {
        let ips = parsed_ips(&["8.8.8.8", "1.1.1.1", "2001:4860:4860::8888"]);
        assert!(validate_resolved_ips(0, &ips, false).is_ok());
    }

    #[test]
    fn resolved_ip_validation_rejects_non_public_addresses_by_default() {
        for address in [
            "10.0.0.1",
            "127.0.0.1",
            "169.254.169.254",
            "fd00::1",
            "::1",
            "fe80::1",
        ] {
            let ips = parsed_ips(&[address]);
            assert!(
                validate_resolved_ips(0, &ips, false).is_err(),
                "{address} should be rejected"
            );
        }
    }

    #[test]
    fn resolved_ip_validation_allows_non_public_addresses_when_opted_in() {
        let ips = parsed_ips(&[
            "10.0.0.1",
            "127.0.0.1",
            "169.254.169.254",
            "fd00::1",
            "::1",
            "fe80::1",
        ]);
        assert!(validate_resolved_ips(0, &ips, true).is_ok());
    }

    #[test]
    fn resolved_ip_validation_always_rejects_invalid_addresses() {
        for address in ["0.0.0.0", "255.255.255.255", "224.0.0.1", "::", "ff02::1"] {
            let ips = parsed_ips(&[address]);
            assert!(
                validate_resolved_ips(0, &ips, true).is_err(),
                "{address} should be rejected"
            );
        }
    }

    #[test]
    fn resolved_ip_validation_rejects_mixed_public_and_private_results() {
        let ips = parsed_ips(&["8.8.8.8", "10.0.0.1"]);
        assert!(validate_resolved_ips(0, &ips, false).is_err());
        assert!(validate_resolved_ips(0, &ips, true).is_ok());
    }

    #[test]
    fn resolved_ip_validation_rejects_empty_results() {
        assert!(validate_resolved_ips(0, &[], false).is_err());
    }

    #[test]
    fn resolved_ip_validation_errors_do_not_expose_endpoint_material() {
        let ips = parsed_ips(&["10.23.45.67"]);
        let error = validate_resolved_ips(7, &ips, false)
            .unwrap_err()
            .to_string();

        assert!(error.contains("endpoint 7"));
        for leaked in [
            "sensitive.example.test",
            "https://sensitive.example.test/hook?token=query-token",
            "query-token",
            "super-secret-value",
            "10.23.45.67",
        ] {
            assert!(!error.contains(leaked), "error must not contain {leaked}");
        }
    }

    #[test]
    fn url_with_embedded_credentials_is_rejected() {
        for allow_private_urls in [false, true] {
            let config =
                endpoint_with_url("http://user:pass@example.com/webhook", allow_private_urls);
            let error = compile_endpoint_error(&config);
            assert!(error.contains("must not embed credentials"));
        }
    }

    #[test]
    fn private_ip_is_rejected_by_default() {
        for url in [
            "http://10.0.0.1/webhook",
            "http://172.16.0.1/webhook",
            "http://192.168.1.1/webhook",
            "http://[fd00::1]/webhook",
        ] {
            let config = endpoint_with_url(url, false);
            assert!(
                compile_endpoint(&config, 0).is_err(),
                "{url} should be rejected"
            );
        }
    }

    #[test]
    fn loopback_is_rejected_by_default() {
        for url in [
            "http://127.0.0.1:18080/webhook",
            "http://[::1]:18080/webhook",
            "http://localhost:18080/webhook",
            "http://[::ffff:127.0.0.1]/webhook",
        ] {
            let config = endpoint_with_url(url, false);
            assert!(
                compile_endpoint(&config, 0).is_err(),
                "{url} should be rejected"
            );
        }
    }

    #[test]
    fn link_local_ip_is_rejected_by_default() {
        for url in ["http://169.254.1.1/webhook", "http://[fe80::1]/webhook"] {
            let config = endpoint_with_url(url, false);
            assert!(
                compile_endpoint(&config, 0).is_err(),
                "{url} should be rejected"
            );
        }
    }

    #[test]
    fn allow_private_urls_permits_local_receiver_address() {
        for url in [
            "http://127.0.0.1:18080/webhook",
            "http://localhost:18080/webhook",
            "http://192.168.1.1/webhook",
        ] {
            let config = endpoint_with_url(url, true);
            assert!(
                compile_endpoint(&config, 0).is_ok(),
                "{url} should be permitted"
            );
        }

        let public = endpoint_with_url("https://hooks.example.com/webhook", false);
        assert!(compile_endpoint(&public, 0).is_ok());
    }

    #[test]
    fn unspecified_broadcast_and_multicast_are_rejected_even_when_allowed() {
        for url in [
            "http://0.0.0.0/webhook",
            "http://[::]/webhook",
            "http://255.255.255.255/webhook",
            "http://224.0.0.1/webhook",
            "http://[ff02::1]/webhook",
        ] {
            let config = endpoint_with_url(url, true);
            assert!(
                compile_endpoint(&config, 0).is_err(),
                "{url} should be rejected"
            );
        }
    }

    #[test]
    fn url_validation_errors_do_not_expose_url_userinfo_or_secret() {
        let mut config =
            endpoint_with_url("http://svc-user:super-secret-pass@10.1.2.3/webhook", false);
        config.secret = Some("endpoint-signing-secret".to_string());
        let error = compile_endpoint_error(&config);
        for leaked in [
            "svc-user",
            "super-secret-pass",
            "10.1.2.3",
            "endpoint-signing-secret",
        ] {
            assert!(!error.contains(leaked), "error must not contain {leaked}");
        }

        let mut config = endpoint_with_url("http://192.168.7.9:9999/hook?token=abc", false);
        config.secret = Some("endpoint-signing-secret".to_string());
        let error = compile_endpoint_error(&config);
        for leaked in [
            "192.168.7.9",
            "9999",
            "token=abc",
            "endpoint-signing-secret",
        ] {
            assert!(!error.contains(leaked), "error must not contain {leaked}");
        }
    }

    #[test]
    fn endpoint_debug_redacts_url_and_secret_material() {
        let url = concat!(
            "https://debug-user:debug-password@private.example.test/webhook/path",
            "?token=query-token&authorization=Bearer%20header-value"
        );
        let endpoint = Endpoint {
            index: 7,
            url: Url::parse(url).unwrap(),
            events: vec!["sandbox.created".to_string()],
            secret: Some("endpoint-signing-secret".to_string()),
        };

        let debug = format!("{endpoint:?}");

        assert!(debug.contains("index: 7"));
        assert!(debug.contains("sandbox.created"));
        assert!(debug.contains("secret_configured: true"));
        assert!(!debug.contains(url));
        for leaked in [
            "private.example.test",
            "debug-user",
            "debug-password",
            "/webhook/path",
            "query-token",
            "header-value",
            "endpoint-signing-secret",
        ] {
            assert!(
                !debug.contains(leaked),
                "debug output must not contain {leaked}"
            );
        }
    }

    #[test]
    fn hmac_signature_has_stable_v1_lowercase_hex_format() {
        let secret = "test-secret";
        let timestamp = "1710000000";
        let delivery_id = "550e8400-e29b-41d4-a716-446655440000";
        let body = br#"{"id":"delivery-1","event":"sandbox.created"}"#;

        let signature = sign_payload(secret, timestamp, delivery_id, body);

        let mut expected_mac = Hmac::<Sha256>::new_from_slice(secret.as_bytes()).unwrap();
        expected_mac.update(timestamp.as_bytes());
        expected_mac.update(b".");
        expected_mac.update(delivery_id.as_bytes());
        expected_mac.update(b".");
        expected_mac.update(body);
        let expected = format!("v1={}", hex::encode(expected_mac.finalize().into_bytes()));

        assert_eq!(signature, expected);
        assert_eq!(signature.len(), 67);
        assert!(signature[3..]
            .chars()
            .all(|character| character.is_ascii_digit() || ('a'..='f').contains(&character)));

        let unsigned_endpoint = compile_endpoint(&endpoint(&["sandbox.created"]), 0).unwrap();
        let unsigned_event = test_event();
        let unsigned_fields = sanitized_fields(&unsigned_event.fields);
        let unsigned_delivery = Delivery::new(
            &unsigned_event,
            &unsigned_endpoint,
            &unsigned_fields,
            MAX_WEBHOOK_PAYLOAD_BYTES,
        )
        .unwrap();
        assert!(unsigned_delivery.signature.is_none());
    }

    #[test]
    fn hmac_is_computed_over_the_exact_checked_bytes() {
        let secret = "checked-body-secret";
        let mut endpoint_config = endpoint(&["*"]);
        endpoint_config.secret = Some(secret.to_string());
        let compiled = compile_endpoint(&endpoint_config, 0).unwrap();
        let event = test_event();
        let fields = sanitized_fields(&event.fields);
        let wide = Delivery::new(&event, &compiled, &fields, MAX_WEBHOOK_PAYLOAD_BYTES).unwrap();
        let payload_bytes = wide.body.len();

        let delivery = Delivery::new(&event, &compiled, &fields, payload_bytes).unwrap();
        let expected = sign_payload(
            secret,
            &delivery.timestamp,
            &delivery.id,
            delivery.body.as_ref(),
        );

        assert_eq!(delivery.body.len(), payload_bytes);
        assert_eq!(delivery.signature.as_deref(), Some(expected.as_str()));
    }

    #[test]
    fn retryable_statuses_are_classified_correctly() {
        for status in [408, 425, 429, 500, 503] {
            assert!(is_retryable_status(StatusCode::from_u16(status).unwrap()));
        }
        for status in [200, 201, 301, 302, 400, 401, 403, 404] {
            assert!(!is_retryable_status(StatusCode::from_u16(status).unwrap()));
        }
    }

    #[test]
    fn payload_serialization_flattens_fields_and_includes_delivery_metadata() {
        let event = test_event();
        let compiled = compile_endpoint(&endpoint(&["*"]), 0).unwrap();
        let fields = sanitized_fields(&event.fields);
        let delivery =
            Delivery::new(&event, &compiled, &fields, MAX_WEBHOOK_PAYLOAD_BYTES).unwrap();
        let payload: Value = serde_json::from_slice(delivery.body.as_ref()).unwrap();

        assert_eq!(payload["id"], delivery.id);
        assert!(payload["timestamp"].is_string());
        assert_eq!(payload["level"], "info");
        assert_eq!(payload["event"], "sandbox.created");
        assert_eq!(payload["sandbox_id"], "sandbox-123");
        assert_eq!(payload["template_id"], "template-456");
        assert!(payload.get("fields").is_none());
    }

    #[test]
    fn payload_size_limit_is_exact_at_boundary() {
        let event = test_event();
        let compiled = compile_endpoint(&endpoint(&["*"]), 0).unwrap();
        let fields = sanitized_fields(&event.fields);
        let wide = Delivery::new(&event, &compiled, &fields, MAX_WEBHOOK_PAYLOAD_BYTES).unwrap();
        let payload_bytes = wide.body.len();

        assert!(payload_bytes > 0);
        assert!(Delivery::new(&event, &compiled, &fields, payload_bytes).is_ok());
        assert!(matches!(
            Delivery::new(&event, &compiled, &fields, payload_bytes - 1),
            Err(DeliveryBuildError::PayloadTooLarge {
                payload_bytes: actual,
            }) if actual == payload_bytes
        ));
    }

    #[test]
    fn payload_size_is_measured_in_serialized_utf8_bytes() {
        let ascii_value = "abcdef";
        let cjk_value = "沙箱事件负载";
        assert_eq!(ascii_value.chars().count(), 6);
        assert_eq!(cjk_value.chars().count(), 6);
        assert_eq!(cjk_value.len() - ascii_value.len(), 12);

        let base = LogEvent::new(LogLevel::Info, "sandbox.created");
        let ascii_event = base.clone().field("payload", ascii_value);
        let cjk_event = base.field("payload", cjk_value);
        let ascii_fields = sanitized_fields(&ascii_event.fields);
        let cjk_fields = sanitized_fields(&cjk_event.fields);
        let compiled = compile_endpoint(&endpoint(&["*"]), 0).unwrap();
        let ascii_delivery = Delivery::new(
            &ascii_event,
            &compiled,
            &ascii_fields,
            MAX_WEBHOOK_PAYLOAD_BYTES,
        )
        .unwrap();
        let cjk_delivery = Delivery::new(
            &cjk_event,
            &compiled,
            &cjk_fields,
            MAX_WEBHOOK_PAYLOAD_BYTES,
        )
        .unwrap();
        let common_limit = ascii_delivery.body.len();

        assert_eq!(cjk_delivery.body.len(), common_limit + 12);
        assert!(Delivery::new(&ascii_event, &compiled, &ascii_fields, common_limit).is_ok());
        assert!(matches!(
            Delivery::new(&cjk_event, &compiled, &cjk_fields, common_limit),
            Err(DeliveryBuildError::PayloadTooLarge { payload_bytes })
                if payload_bytes == cjk_delivery.body.len()
        ));
    }

    #[test]
    fn payload_size_counts_json_escape_expansion() {
        let plain_value = "abcdef";
        let escaped_value = "a\"b\\c\n";
        assert_eq!(plain_value.len(), escaped_value.len());

        let base = LogEvent::new(LogLevel::Info, "sandbox.created");
        let plain_event = base.clone().field("payload", plain_value);
        let escaped_event = base.field("payload", escaped_value);
        let plain_fields = sanitized_fields(&plain_event.fields);
        let escaped_fields = sanitized_fields(&escaped_event.fields);
        let compiled = compile_endpoint(&endpoint(&["*"]), 0).unwrap();
        let plain_delivery = Delivery::new(
            &plain_event,
            &compiled,
            &plain_fields,
            MAX_WEBHOOK_PAYLOAD_BYTES,
        )
        .unwrap();
        let escaped_delivery = Delivery::new(
            &escaped_event,
            &compiled,
            &escaped_fields,
            MAX_WEBHOOK_PAYLOAD_BYTES,
        )
        .unwrap();
        let common_limit = plain_delivery.body.len();
        let raw_json = std::str::from_utf8(escaped_delivery.body.as_ref()).unwrap();

        assert_eq!(escaped_delivery.body.len(), common_limit + 3);
        assert!(raw_json.contains(r#""payload":"a\"b\\c\n""#));
        assert!(Delivery::new(&plain_event, &compiled, &plain_fields, common_limit).is_ok());
        assert!(matches!(
            Delivery::new(&escaped_event, &compiled, &escaped_fields, common_limit),
            Err(DeliveryBuildError::PayloadTooLarge { payload_bytes })
                if payload_bytes == escaped_delivery.body.len()
        ));
    }

    #[test]
    fn delivery_metadata_counts_toward_payload_size() {
        let event = LogEvent::new(LogLevel::Info, "sandbox.created");
        let fields = sanitized_fields(&event.fields);
        let compiled = compile_endpoint(&endpoint(&["*"]), 0).unwrap();

        assert!(matches!(
            Delivery::new(&event, &compiled, &fields, 10),
            Err(DeliveryBuildError::PayloadTooLarge { .. })
        ));

        let delivery =
            Delivery::new(&event, &compiled, &fields, MAX_WEBHOOK_PAYLOAD_BYTES).unwrap();
        let payload: Value = serde_json::from_slice(delivery.body.as_ref()).unwrap();
        assert_eq!(payload.as_object().unwrap().len(), 4);
        assert_eq!(payload["id"], delivery.id);
        assert!(payload["timestamp"].is_string());
        assert_eq!(payload["level"], "info");
        assert_eq!(payload["event"], "sandbox.created");
    }

    #[test]
    fn four_lifecycle_events_fit_well_under_default_limit() {
        let compiled = compile_endpoint(&endpoint(&["*"]), 0).unwrap();
        let default_limit = WebhookConfig::default().max_payload_bytes;

        for event_name in DEFAULT_LIFECYCLE_EVENTS {
            let event = LogEvent::new(LogLevel::Info, event_name)
                .field("sandbox_id", "sandbox-123")
                .field("template_id", "template-456");
            let fields = sanitized_fields(&event.fields);
            let delivery = Delivery::new(&event, &compiled, &fields, default_limit).unwrap();

            assert!(
                delivery.body.len() < default_limit / 100,
                "{event_name} payload should remain well below the default limit"
            );
        }
    }

    fn nested_object(levels: usize, leaf: Value) -> Value {
        (0..levels).fold(leaf, |value, _| serde_json::json!({"child": value}))
    }

    fn nested_array(levels: usize, leaf: Value) -> Value {
        (0..levels).fold(leaf, |value, _| Value::Array(vec![value]))
    }

    fn serialized_value_contains(value: &Value, text: &str) -> bool {
        serde_json::to_string(value).unwrap().contains(text)
    }

    #[test]
    fn sanitize_value_preserves_normal_nesting_and_redacts_sensitive_fields() {
        let value = serde_json::json!({
            "metadata": {
                "visible": "kept",
                "accessToken": "removed-token",
                "nested": {
                    "region": "visible-region",
                    "clientSecret": "removed-secret"
                }
            }
        });

        let sanitized = sanitize_value(&value);

        assert_eq!(sanitized["metadata"]["visible"], "kept");
        assert_eq!(sanitized["metadata"]["nested"]["region"], "visible-region");
        assert!(sanitized["metadata"].get("accessToken").is_none());
        assert!(sanitized["metadata"]["nested"]
            .get("clientSecret")
            .is_none());
    }

    #[test]
    fn sanitize_value_truncates_deeply_nested_objects_without_exposing_leaf_values() {
        let value = nested_object(
            MAX_SANITIZE_DEPTH + 3,
            serde_json::json!({"value": "deep-object-secret-value"}),
        );

        let sanitized = sanitize_value(&value);

        assert!(serialized_value_contains(
            &sanitized,
            SANITIZE_DEPTH_TRUNCATION_PLACEHOLDER
        ));
        assert!(!serialized_value_contains(
            &sanitized,
            "deep-object-secret-value"
        ));
    }

    #[test]
    fn sanitize_value_truncates_deeply_nested_arrays_without_exposing_leaf_values() {
        let value = nested_array(
            MAX_SANITIZE_DEPTH + 3,
            Value::String("deep-array-secret-value".to_string()),
        );

        let sanitized = sanitize_value(&value);

        assert!(serialized_value_contains(
            &sanitized,
            SANITIZE_DEPTH_TRUNCATION_PLACEHOLDER
        ));
        assert!(!serialized_value_contains(
            &sanitized,
            "deep-array-secret-value"
        ));
    }

    #[test]
    fn sanitize_value_enforces_depth_limit_at_composite_boundary() {
        let within_limit = nested_object(
            MAX_SANITIZE_DEPTH,
            Value::String("boundary-value".to_string()),
        );
        let beyond_limit = nested_object(
            MAX_SANITIZE_DEPTH + 1,
            Value::String("truncated-boundary-value".to_string()),
        );

        let sanitized_within_limit = sanitize_value(&within_limit);
        let sanitized_beyond_limit = sanitize_value(&beyond_limit);

        assert!(serialized_value_contains(
            &sanitized_within_limit,
            "boundary-value"
        ));
        assert!(!serialized_value_contains(
            &sanitized_within_limit,
            SANITIZE_DEPTH_TRUNCATION_PLACEHOLDER
        ));
        assert!(serialized_value_contains(
            &sanitized_beyond_limit,
            SANITIZE_DEPTH_TRUNCATION_PLACEHOLDER
        ));
        assert!(!serialized_value_contains(
            &sanitized_beyond_limit,
            "truncated-boundary-value"
        ));
    }

    #[test]
    fn payload_serialization_removes_sensitive_fields_recursively() {
        let event = test_event()
            .field("access_token", "top-level-token")
            .field("authToken", "top-level-camel-token")
            .field_value(
                "metadata",
                serde_json::json!({
                    "safe": "visible",
                    "tokenBucketConfig": "visible-camel-metadata",
                    "secret": "nested-secret",
                    "clientSecret": "nested-client-secret",
                    "credentials": {
                        "password": "nested-password"
                    },
                    "integration": {
                        "webhookSignature": "nested-signature",
                        "details": {
                            "gitlabAccessToken": "deeply-nested-token",
                            "region": "visible-region"
                        }
                    }
                }),
            );
        let compiled = compile_endpoint(&endpoint(&["*"]), 0).unwrap();
        let fields = sanitized_fields(&event.fields);
        let delivery =
            Delivery::new(&event, &compiled, &fields, MAX_WEBHOOK_PAYLOAD_BYTES).unwrap();
        let payload: Value = serde_json::from_slice(delivery.body.as_ref()).unwrap();

        assert!(payload.get("access_token").is_none());
        assert!(payload.get("authToken").is_none());
        assert_eq!(payload["metadata"]["safe"], "visible");
        assert_eq!(
            payload["metadata"]["tokenBucketConfig"],
            "visible-camel-metadata"
        );
        assert!(payload["metadata"].get("secret").is_none());
        assert!(payload["metadata"].get("clientSecret").is_none());
        assert!(payload["metadata"].get("credentials").is_none());
        assert!(payload["metadata"]["integration"]
            .get("webhookSignature")
            .is_none());
        assert!(payload["metadata"]["integration"]["details"]
            .get("gitlabAccessToken")
            .is_none());
        assert_eq!(
            payload["metadata"]["integration"]["details"]["region"],
            "visible-region"
        );
        for leaked in [
            "top-level-camel-token",
            "nested-client-secret",
            "nested-password",
            "nested-signature",
            "deeply-nested-token",
        ] {
            assert!(!delivery
                .body
                .windows(leaked.len())
                .any(|window| window == leaked.as_bytes()));
        }
    }

    #[derive(Clone)]
    struct MockState {
        statuses: Arc<Mutex<VecDeque<StatusCode>>>,
        calls: Arc<AtomicUsize>,
        requests: Arc<Mutex<Vec<(HeaderMap, Bytes)>>>,
    }

    async fn mock_webhook(
        State(state): State<MockState>,
        headers: HeaderMap,
        body: Bytes,
    ) -> StatusCode {
        state.calls.fetch_add(1, Ordering::SeqCst);
        state.requests.lock().unwrap().push((headers, body));
        state
            .statuses
            .lock()
            .unwrap()
            .pop_front()
            .unwrap_or(StatusCode::OK)
    }

    async fn spawn_mock_server(
        statuses: Vec<StatusCode>,
    ) -> (
        String,
        Arc<AtomicUsize>,
        Arc<Mutex<Vec<(HeaderMap, Bytes)>>>,
    ) {
        let state = MockState {
            statuses: Arc::new(Mutex::new(statuses.into())),
            calls: Arc::new(AtomicUsize::new(0)),
            requests: Arc::new(Mutex::new(Vec::new())),
        };
        let calls = state.calls.clone();
        let requests = state.requests.clone();
        let app = Router::new()
            .route("/webhook", post(mock_webhook))
            .route("/__ready", get(|| async { StatusCode::NO_CONTENT }))
            .with_state(state);
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();

        tokio::spawn(async move {
            let _ = axum::serve(listener, app).await;
        });

        let readiness_url = format!("http://{address}/__ready");
        let readiness_client = reqwest::Client::builder()
            .no_proxy()
            .build()
            .expect("readiness client should build");
        let mut ready = false;
        for attempt in 0..100 {
            if matches!(
                readiness_client.get(&readiness_url).send().await,
                Ok(response) if response.status().is_success()
            ) {
                ready = true;
                break;
            }
            if attempt < 99 {
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
        }
        assert!(
            ready,
            "mock webhook server did not become ready after 100 attempts"
        );

        (format!("http://{address}/webhook"), calls, requests)
    }

    async fn delivery_attempts(statuses: Vec<StatusCode>, max_retries: usize) -> usize {
        let (url, calls, _requests) = spawn_mock_server(statuses).await;
        let logger = HttpLogger::new(test_config(url, 8, max_retries))
            .await
            .unwrap();
        logger.log(test_event()).await;
        logger.flush().await;
        calls.load(Ordering::SeqCst)
    }

    #[tokio::test]
    async fn successful_2xx_response_is_not_retried() {
        assert_eq!(
            delivery_attempts(
                vec![StatusCode::CREATED, StatusCode::INTERNAL_SERVER_ERROR],
                3
            )
            .await,
            1
        );
    }

    #[tokio::test]
    async fn retryable_500_and_429_responses_are_retried() {
        assert_eq!(
            delivery_attempts(vec![StatusCode::INTERNAL_SERVER_ERROR, StatusCode::OK], 3).await,
            2
        );
        assert_eq!(
            delivery_attempts(vec![StatusCode::TOO_MANY_REQUESTS, StatusCode::OK], 3).await,
            2
        );
    }

    #[tokio::test]
    async fn ordinary_4xx_response_is_not_retried() {
        assert_eq!(
            delivery_attempts(vec![StatusCode::BAD_REQUEST, StatusCode::OK], 3).await,
            1
        );
    }

    #[tokio::test]
    async fn delivered_request_headers_match_documented_spec() {
        let secret = "header-test-secret";
        let (url, calls, requests) = spawn_mock_server(vec![StatusCode::OK]).await;
        let mut config = test_config(url, 8, 0);
        config.endpoints[0].secret = Some(secret.to_string());
        let logger = HttpLogger::new(config).await.unwrap();

        logger.log(test_event()).await;
        logger.flush().await;

        assert_eq!(calls.load(Ordering::SeqCst), 1);
        let captured = requests.lock().unwrap();
        assert_eq!(captured.len(), 1, "readiness probes must not be captured");
        let (headers, body) = &captured[0];

        assert_eq!(
            headers.get(reqwest::header::CONTENT_TYPE).unwrap(),
            "application/json"
        );
        assert_eq!(
            headers.get(reqwest::header::USER_AGENT).unwrap(),
            WEBHOOK_USER_AGENT
        );
        assert_eq!(headers.get(HEADER_EVENT).unwrap(), "sandbox.created");

        let delivery_id = headers.get(HEADER_DELIVERY).unwrap().to_str().unwrap();
        Uuid::parse_str(delivery_id).expect("delivery header must be a UUID");
        let payload: Value = serde_json::from_slice(body).unwrap();
        assert_eq!(payload["id"], delivery_id);

        let timestamp = headers.get(HEADER_TIMESTAMP).unwrap().to_str().unwrap();
        timestamp
            .parse::<i64>()
            .expect("timestamp header must be unix seconds");

        let signature = headers.get(HEADER_SIGNATURE).unwrap().to_str().unwrap();
        assert_eq!(
            signature,
            sign_payload(secret, timestamp, delivery_id, body)
        );
    }

    #[tokio::test]
    async fn oversized_event_sends_no_request_and_no_retry() {
        let statuses = vec![
            StatusCode::INTERNAL_SERVER_ERROR,
            StatusCode::INTERNAL_SERVER_ERROR,
            StatusCode::OK,
        ];
        let (url, calls, requests) = spawn_mock_server(statuses).await;
        let mut config = test_config(url, 8, 3);
        config.max_payload_bytes = 8;
        let logger = HttpLogger::new(config).await.unwrap();

        logger.log(test_event()).await;
        logger.flush().await;

        assert_eq!(calls.load(Ordering::SeqCst), 0);
        assert!(requests.lock().unwrap().is_empty());
    }

    #[tokio::test]
    async fn oversized_event_is_dropped_for_all_endpoints_deterministically() {
        let secret = "second-endpoint-secret";
        let statuses = vec![StatusCode::OK, StatusCode::OK];
        let (url, calls, requests) = spawn_mock_server(statuses).await;
        let event = test_event();

        let mut tiny_config = test_config(url.clone(), 8, 0);
        let mut tiny_second_endpoint = tiny_config.endpoints[0].clone();
        tiny_second_endpoint.secret = Some(secret.to_string());
        tiny_config.endpoints.push(tiny_second_endpoint);
        tiny_config.max_payload_bytes = 8;
        let tiny_logger = HttpLogger::new(tiny_config).await.unwrap();

        tiny_logger.log(event.clone()).await;
        tiny_logger.flush().await;
        assert_eq!(calls.load(Ordering::SeqCst), 0);
        assert!(requests.lock().unwrap().is_empty());
        drop(tiny_logger);

        let mut wide_config = test_config(url, 8, 0);
        let mut wide_second_endpoint = wide_config.endpoints[0].clone();
        wide_second_endpoint.secret = Some(secret.to_string());
        wide_config.endpoints.push(wide_second_endpoint);
        wide_config.max_payload_bytes = MAX_WEBHOOK_PAYLOAD_BYTES;
        let wide_logger = HttpLogger::new(wide_config).await.unwrap();

        wide_logger.log(event).await;
        wide_logger.flush().await;

        assert_eq!(calls.load(Ordering::SeqCst), 2);
        let captured = requests.lock().unwrap();
        assert_eq!(captured.len(), 2, "readiness probes must not be captured");
        assert_eq!(captured[0].1.len(), captured[1].1.len());

        let mut delivery_ids = Vec::with_capacity(captured.len());
        let mut normalized_payloads = Vec::with_capacity(captured.len());
        for (headers, body) in &*captured {
            let mut payload: Value = serde_json::from_slice(body).unwrap();
            assert_eq!(payload["event"], "sandbox.created");
            assert_eq!(payload["level"], "info");
            assert_eq!(payload["sandbox_id"], "sandbox-123");
            assert_eq!(payload["template_id"], "template-456");
            assert!(payload.get("fields").is_none());
            let delivery_id = payload["id"].as_str().unwrap().to_string();
            assert_eq!(
                delivery_id,
                headers.get(HEADER_DELIVERY).unwrap().to_str().unwrap()
            );
            delivery_ids.push(delivery_id);
            assert!(payload.as_object_mut().unwrap().remove("id").is_some());
            normalized_payloads.push(payload);
        }
        assert_ne!(delivery_ids[0], delivery_ids[1]);
        assert_eq!(normalized_payloads[0], normalized_payloads[1]);
        assert_eq!(
            captured
                .iter()
                .filter(|(headers, _)| headers.contains_key(HEADER_SIGNATURE))
                .count(),
            1
        );
    }

    #[tokio::test(flavor = "current_thread")]
    async fn full_queue_drops_events_without_blocking_log() {
        let (url, calls, _requests) = spawn_mock_server(vec![StatusCode::OK]).await;
        let logger = HttpLogger::new(test_config(url, 1, 0)).await.unwrap();

        timeout(Duration::from_secs(1), async {
            for _ in 0..20 {
                logger.log(test_event()).await;
            }
        })
        .await
        .expect("log() must not wait for queue capacity");

        logger.flush().await;
        assert_eq!(calls.load(Ordering::SeqCst), 1);
    }

    /// Mock server with a `/gated` route that parks each request until the
    /// test releases a `gate` permit, and an `/open` route that responds
    /// immediately. Arrival semaphores start at zero permits and gain one per
    /// incoming request, so tests await the k-th arrival deterministically
    /// instead of sleeping.
    #[derive(Clone)]
    struct GatedMockState {
        gate: Arc<Semaphore>,
        gated_arrivals: Arc<Semaphore>,
        gated_completions: Arc<AtomicUsize>,
        open_arrivals: Arc<Semaphore>,
        open_calls: Arc<AtomicUsize>,
    }

    async fn gated_webhook(State(state): State<GatedMockState>) -> StatusCode {
        state.gated_arrivals.add_permits(1);
        match state.gate.acquire().await {
            // `forget` consumes the permit so each release admits exactly one
            // parked request.
            Ok(permit) => permit.forget(),
            Err(_) => return StatusCode::SERVICE_UNAVAILABLE,
        }
        state.gated_completions.fetch_add(1, Ordering::SeqCst);
        StatusCode::OK
    }

    async fn open_webhook(State(state): State<GatedMockState>) -> StatusCode {
        state.open_calls.fetch_add(1, Ordering::SeqCst);
        state.open_arrivals.add_permits(1);
        StatusCode::OK
    }

    async fn spawn_gated_mock_server() -> (String, String, GatedMockState) {
        let state = GatedMockState {
            gate: Arc::new(Semaphore::new(0)),
            gated_arrivals: Arc::new(Semaphore::new(0)),
            gated_completions: Arc::new(AtomicUsize::new(0)),
            open_arrivals: Arc::new(Semaphore::new(0)),
            open_calls: Arc::new(AtomicUsize::new(0)),
        };
        let app = Router::new()
            .route("/gated", post(gated_webhook))
            .route("/open", post(open_webhook))
            .route("/__ready", get(|| async { StatusCode::NO_CONTENT }))
            .with_state(state.clone());
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();

        tokio::spawn(async move {
            let _ = axum::serve(listener, app).await;
        });

        let readiness_url = format!("http://{address}/__ready");
        let readiness_client = reqwest::Client::builder()
            .no_proxy()
            .build()
            .expect("readiness client should build");
        let mut ready = false;
        for attempt in 0..100 {
            if matches!(
                readiness_client.get(&readiness_url).send().await,
                Ok(response) if response.status().is_success()
            ) {
                ready = true;
                break;
            }
            if attempt < 99 {
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
        }
        assert!(
            ready,
            "gated mock webhook server did not become ready after 100 attempts"
        );

        (
            format!("http://{address}/gated"),
            format!("http://{address}/open"),
            state,
        )
    }

    /// Waits for `count` arrivals, consuming their permits so every arrival is
    /// counted exactly once. The timeout is only a hang guard; the semaphore
    /// is the synchronization mechanism.
    async fn acquire_arrivals(arrivals: &Semaphore, count: u32) {
        timeout(Duration::from_secs(5), arrivals.acquire_many(count))
            .await
            .expect("timed out waiting for expected webhook requests")
            .expect("arrival semaphore closed")
            .forget();
    }

    fn gated_test_config(url: String, queue_capacity: usize) -> WebhookConfig {
        let mut config = test_config(url, queue_capacity, 0);
        // Generous limits: parked requests must never hit the client timeout,
        // and flush must never abort deliveries, before the test releases the
        // gate. The gate is always released, so these limits are never
        // approached in a passing run.
        config.timeout_secs = 30;
        config.flush_timeout_secs = 30;
        config.max_concurrency = 16;
        config
    }

    #[tokio::test]
    async fn saturated_endpoint_does_not_block_healthy_endpoint() {
        let (gated_url, open_url, mock) = spawn_gated_mock_server().await;
        // max_concurrency (16) deliberately exceeds the endpoint budget (4):
        // this test isolates per-endpoint admission, while parked requests
        // still hold global HTTP permits.
        let mut config = gated_test_config(gated_url, 4);
        let mut open_endpoint = config.endpoints[0].clone();
        open_endpoint.url = open_url;
        open_endpoint.events = vec!["sandbox.created".to_string()];
        config.endpoints.push(open_endpoint);
        let logger = HttpLogger::new(config).await.unwrap();

        for _ in 0..4 {
            logger
                .log(LogEvent::new(LogLevel::Info, "sandbox.deleted").field("sandbox_id", "s-1"))
                .await;
            acquire_arrivals(&mock.gated_arrivals, 1).await;
        }

        logger.log(test_event()).await;
        acquire_arrivals(&mock.open_arrivals, 1).await;
        assert_eq!(mock.gated_completions.load(Ordering::SeqCst), 0);

        mock.gate.add_permits(64);
        logger.flush().await;
        assert_eq!(mock.gated_completions.load(Ordering::SeqCst), 4);
        assert_eq!(mock.open_calls.load(Ordering::SeqCst), 1);
        assert_eq!(
            mock.gated_arrivals.available_permits(),
            0,
            "the saturated endpoint must not receive the fifth delivery"
        );
    }

    #[tokio::test]
    async fn endpoint_outstanding_deliveries_never_exceed_budget() {
        let (gated_url, open_url, mock) = spawn_gated_mock_server().await;
        // queue_capacity 4 also derives a per-endpoint outstanding budget of
        // 4; max_concurrency (16) keeps admission the binding limit. The
        // gated endpoint subscribes to all events; the open endpoint only to
        // "sandbox.created", so fill events park on the gated endpoint alone.
        let mut config = gated_test_config(gated_url, 4);
        let mut open_endpoint = config.endpoints[0].clone();
        open_endpoint.url = open_url;
        open_endpoint.events = vec!["sandbox.created".to_string()];
        config.endpoints.push(open_endpoint);
        let logger = HttpLogger::new(config).await.unwrap();

        // Fill the gated endpoint's budget one event at a time; observing
        // each arrival proves the dispatcher admitted and spawned that
        // delivery.
        for _ in 0..4 {
            logger
                .log(LogEvent::new(LogLevel::Info, "sandbox.deleted").field("sandbox_id", "s-1"))
                .await;
            acquire_arrivals(&mock.gated_arrivals, 1).await;
        }

        // These exceed the gated endpoint's budget and must be rejected
        // without blocking the dispatcher. Each event is adjudicated for the
        // gated endpoint before its open-endpoint delivery is spawned (config
        // order within one spawn_deliveries call), so the open arrivals below
        // prove all three rejections happened while the budget was still
        // fully held - no permit could free up before the gate opens.
        for _ in 0..3 {
            logger.log(test_event()).await;
        }
        acquire_arrivals(&mock.open_arrivals, 3).await;

        mock.gate.add_permits(64);
        logger.flush().await;

        assert_eq!(mock.gated_completions.load(Ordering::SeqCst), 4);
        assert_eq!(mock.open_calls.load(Ordering::SeqCst), 3);
        assert_eq!(
            mock.gated_arrivals.available_permits(),
            0,
            "a delivery beyond the endpoint budget must never reach the endpoint"
        );
    }

    #[tokio::test(flavor = "current_thread")]
    async fn blocked_endpoint_does_not_block_log() {
        let (gated_url, _open_url, mock) = spawn_gated_mock_server().await;
        let logger = HttpLogger::new(gated_test_config(gated_url, 1))
            .await
            .unwrap();

        logger.log(test_event()).await;
        acquire_arrivals(&mock.gated_arrivals, 1).await;

        timeout(Duration::from_secs(1), async {
            for _ in 0..20 {
                logger.log(test_event()).await;
            }
        })
        .await
        .expect("log() must not wait for queue capacity or endpoint admission");

        mock.gate.add_permits(64);
        logger.flush().await;
        assert!(mock.gated_completions.load(Ordering::SeqCst) >= 1);
    }

    #[tokio::test]
    async fn oversized_event_consumes_no_endpoint_budget() {
        let slots: Vec<EndpointSlot> = [
            compile_endpoint(&endpoint(&["*"]), 0).unwrap(),
            compile_endpoint(&endpoint(&["*"]), 1).unwrap(),
        ]
        .into_iter()
        .map(|endpoint| EndpointSlot {
            endpoint,
            outstanding: Arc::new(Semaphore::new(1)),
        })
        .collect();
        let client = reqwest::Client::builder().no_proxy().build().unwrap();
        let semaphore = Arc::new(Semaphore::new(4));
        let options = DeliveryOptions {
            max_retries: 0,
            max_payload_bytes: 8,
            initial_backoff_ms: 1,
            max_backoff_ms: 2,
        };
        let mut deliveries = JoinSet::new();

        spawn_deliveries(
            &mut deliveries,
            test_event(),
            &slots,
            &client,
            &semaphore,
            options,
        );

        assert!(deliveries.is_empty(), "no delivery task may be spawned");
        for slot in &slots {
            assert_eq!(slot.outstanding.available_permits(), 1);
        }
    }

    #[tokio::test(flavor = "current_thread")]
    async fn saturated_endpoint_budget_drops_only_its_own_delivery() {
        let saturated = EndpointSlot {
            endpoint: compile_endpoint(&endpoint(&["*"]), 0).unwrap(),
            outstanding: Arc::new(Semaphore::new(0)),
        };
        let available = EndpointSlot {
            endpoint: compile_endpoint(&endpoint(&["*"]), 1).unwrap(),
            outstanding: Arc::new(Semaphore::new(1)),
        };
        let slots = vec![saturated, available];
        let client = reqwest::Client::builder().no_proxy().build().unwrap();
        let semaphore = Arc::new(Semaphore::new(4));
        let options = DeliveryOptions {
            max_retries: 0,
            max_payload_bytes: MAX_WEBHOOK_PAYLOAD_BYTES,
            initial_backoff_ms: 1,
            max_backoff_ms: 2,
        };
        let mut deliveries = JoinSet::new();

        spawn_deliveries(
            &mut deliveries,
            test_event(),
            &slots,
            &client,
            &semaphore,
            options,
        );

        // Only the endpoint with remaining budget received a delivery task;
        // the spawned task has not been polled yet on this current-thread
        // runtime, so its permit is still held.
        assert_eq!(deliveries.len(), 1);
        assert_eq!(slots[1].outstanding.available_permits(), 0);

        // Cancellation must release the outstanding permit via RAII.
        deliveries.abort_all();
        while deliveries.join_next().await.is_some() {}
        assert_eq!(slots[1].outstanding.available_permits(), 1);
    }
}
