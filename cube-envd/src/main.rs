use axum::{
    body::Bytes,
    extract::State,
    http::{header, HeaderMap, HeaderValue, Request, StatusCode},
    middleware::Next,
    response::{Json, Response},
    routing::get,
    Router,
};
use clap::Parser;
use serde::Serialize;
use std::{
    env,
    net::SocketAddr,
    sync::{
        atomic::{AtomicU64, Ordering},
        Arc,
    },
    time::Instant,
};
use tokio::net::TcpListener;
use tracing::{info, warn};
use tracing_subscriber::filter::LevelFilter;

mod auth;
pub mod connect;
mod cors;
mod defaults;
mod files;
mod filesystem;
mod init;
mod metrics;
mod process;
mod processes;
mod pty;
mod termination;
mod watch;

const REQUEST_ID_HEADER: &str = "x-request-id";
static REQUEST_SEQUENCE: AtomicU64 = AtomicU64::new(1);

/// The upstream envd generation cube-envd is protocol-compatible with.
///
/// Clients gate features on the version a sandbox reports (recursive watch,
/// command stdin, default user, closeStdin, octet-stream upload, file
/// metadata, watch `includeEntry`, network mounts), and Cubelet records the
/// `envd --version` output as the template's `envdVersion`. Reporting the
/// crate version (`0.1.0`) made every gate answer "too old", so this constant
/// reports the emulated generation instead; the real implementation version is
/// `env!("CARGO_PKG_VERSION")` and the build's short sha comes from `-commit`.
const ENVD_GENERATION: &str = "0.5.13";

fn version_line() -> String {
    format!("cube-envd {ENVD_GENERATION}")
}

async fn process_start_dispatch(headers: HeaderMap, body: Bytes) -> Response {
    let is_pty = connect::decode_frame(&body)
        .ok()
        .and_then(|(_, payload)| serde_json::from_slice::<serde_json::Value>(&payload).ok())
        .and_then(|value| value.get("pty").cloned())
        .is_some();
    if is_pty {
        pty::start(headers, body).await
    } else {
        process::start(headers, body).await
    }
}

#[derive(Clone)]
struct AppState {
    port: u16,
    started_at: Instant,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct StatusResponse {
    service: &'static str,
    ready: bool,
    version: &'static str,
    commit: &'static str,
    port: u16,
    uptime_seconds: u64,
}

#[derive(Debug, Parser)]
#[command(name = "cube-envd", disable_version_flag = true)]
struct Cli {
    #[arg(long, default_value_t = 49983, value_name = "PORT")]
    port: u16,

    #[arg(long = "isnotfc")]
    is_not_fc: bool,

    #[arg(long = "version")]
    show_version: bool,

    #[arg(long = "commit")]
    show_commit: bool,
}

fn normalized_args(args: impl IntoIterator<Item = String>) -> Vec<String> {
    args.into_iter()
        .map(|arg| match arg.as_str() {
            "-port" => "--port".to_owned(),
            "-isnotfc" => "--isnotfc".to_owned(),
            "-version" => "--version".to_owned(),
            "-commit" => "--commit".to_owned(),
            value if value.starts_with("-port=") => value.replacen("-port=", "--port=", 1),
            _ => arg,
        })
        .collect()
}

/// Drops unknown flags (with a warning) so `ENVD_EXTRA_ARGS` written for the
/// upstream Go envd does not make cube-envd refuse to start.
fn filtered_args(args: impl IntoIterator<Item = String>) -> Vec<String> {
    let mut filtered = Vec::new();
    let mut iter = args.into_iter().peekable();
    while let Some(arg) = iter.next() {
        if filtered.is_empty() {
            filtered.push(arg);
            continue;
        }
        let known = matches!(
            arg.as_str(),
            "--port" | "--isnotfc" | "--version" | "--commit"
        ) || arg.starts_with("--port=");
        if known {
            let is_port_with_value = arg == "--port";
            filtered.push(arg);
            if is_port_with_value {
                if let Some(value) = iter.peek() {
                    if !value.starts_with('-') {
                        filtered.push(iter.next().expect("peeked value exists"));
                    }
                }
            }
            continue;
        }
        eprintln!("cube-envd: ignoring unknown argument {arg}");
        if !arg.starts_with('-') {
            continue;
        }
        if !arg.contains('=') {
            if let Some(value) = iter.peek() {
                if !value.starts_with('-') {
                    let value = iter.next().expect("peeked value exists");
                    eprintln!("cube-envd: ignoring unknown argument {value}");
                }
            }
        }
    }
    filtered
}

async fn health() -> StatusCode {
    StatusCode::NO_CONTENT
}

async fn status(State(state): State<Arc<AppState>>) -> Json<StatusResponse> {
    Json(StatusResponse {
        service: "cube-envd",
        ready: true,
        version: ENVD_GENERATION,
        commit: env!("GIT_SHORT_SHA"),
        port: state.port,
        uptime_seconds: state.started_at.elapsed().as_secs(),
    })
}

async fn codec_guard(request: Request<axum::body::Body>, next: Next) -> Response {
    let path = request.uri().path();
    if (path.contains(".Process/") || path.contains(".Filesystem/"))
        && protobuf_codec_requested(request.headers())
    {
        let payload = serde_json::json!({
            "code": "unimplemented",
            "message": "cube-envd only implements the Connect JSON codec; send Content-Type application/json",
        });
        return Response::builder()
            .status(StatusCode::NOT_IMPLEMENTED)
            .header(header::CONTENT_TYPE, "application/json")
            .body(axum::body::Body::from(payload.to_string()))
            .expect("valid codec error response");
    }
    next.run(request).await
}

/// True when the request asks for the Connect binary-protobuf codec, which
/// cube-envd rejects up front (every known client uses the JSON codec).
fn protobuf_codec_requested(headers: &HeaderMap) -> bool {
    let content_type = headers
        .get(header::CONTENT_TYPE)
        .and_then(|value| value.to_str().ok())
        .map(|value| {
            value
                .split(';')
                .next()
                .unwrap_or("")
                .trim()
                .to_ascii_lowercase()
        });
    matches!(
        content_type.as_deref(),
        Some("application/proto")
            | Some("application/connect+proto")
            | Some("application/grpc")
            | Some("application/grpc+proto")
            | Some("application/grpc-web")
            | Some("application/grpc-web+proto")
    )
}

async fn request_log(request: Request<axum::body::Body>, next: Next) -> Response {
    let method = request.method().clone();
    let path = request.uri().path().to_owned();
    let request_id = request_id(request.headers());
    let started_at = Instant::now();
    let mut response = next.run(request).await;
    let status = response.status();
    let duration_ms = started_at.elapsed().as_secs_f64() * 1_000.0;
    if let Ok(value) = HeaderValue::from_str(&request_id) {
        response
            .headers_mut()
            .insert(header::HeaderName::from_static(REQUEST_ID_HEADER), value);
    }

    if status.is_client_error() || status.is_server_error() {
        warn!(
            %request_id,
            %method,
            %path,
            status = status.as_u16(),
            duration_ms,
            "http request"
        );
    } else {
        info!(
            %request_id,
            %method,
            %path,
            status = status.as_u16(),
            duration_ms,
            "http request"
        );
    }
    response
}

fn request_id(headers: &HeaderMap) -> String {
    headers
        .get(REQUEST_ID_HEADER)
        .and_then(|value| value.to_str().ok())
        .filter(|value| {
            !value.is_empty()
                && value.len() <= 128
                && value
                    .bytes()
                    .all(|byte| byte.is_ascii_alphanumeric() || b"-_.".contains(&byte))
        })
        .map(str::to_owned)
        .unwrap_or_else(|| {
            format!(
                "cube-envd-{}-{}",
                std::process::id(),
                REQUEST_SEQUENCE.fetch_add(1, Ordering::Relaxed)
            )
        })
}

fn init_logging() {
    let level = configured_log_level(env::var("ENVD_LOG_LEVEL").ok().as_deref());

    if configured_log_format(env::var("ENVD_LOG_FORMAT").ok().as_deref()) == LogFormat::Json {
        tracing_subscriber::fmt()
            .json()
            .with_max_level(level)
            .init();
    } else {
        tracing_subscriber::fmt()
            .pretty()
            .with_max_level(level)
            .init();
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum LogFormat {
    Pretty,
    Json,
}

fn configured_log_level(value: Option<&str>) -> LevelFilter {
    value
        .and_then(|value| value.parse::<LevelFilter>().ok())
        .unwrap_or(LevelFilter::INFO)
}

fn configured_log_format(value: Option<&str>) -> LogFormat {
    if value == Some("json") {
        LogFormat::Json
    } else {
        LogFormat::Pretty
    }
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let cli = Cli::parse_from(filtered_args(normalized_args(env::args())));

    if cli.show_version {
        println!("{}", version_line());
        return Ok(());
    }

    if cli.show_commit {
        println!("{}", env!("GIT_SHORT_SHA"));
        return Ok(());
    }

    init_logging();
    if cli.is_not_fc {
        info!("running in is-not-fc mode");
    }

    let address = SocketAddr::from(([0, 0, 0, 0], cli.port));
    let listener = TcpListener::bind(address).await?;
    let state = Arc::new(AppState {
        port: cli.port,
        started_at: Instant::now(),
    });
    info!(
        %address,
        service = "cube-envd",
        generation = ENVD_GENERATION,
        implementation_version = env!("CARGO_PKG_VERSION"),
        commit = env!("GIT_SHORT_SHA"),
        "cube-envd listening"
    );

    let app = Router::new()
        .route("/health", get(health))
        .route("/status", get(status))
        .route("/init", axum::routing::post(init::init))
        .route("/envs", get(init::envs))
        .route("/metrics", get(metrics::metrics))
        .route(
            "/process.Process/Start",
            axum::routing::post(process_start_dispatch),
        )
        .route("/process.Process/List", axum::routing::post(process::list))
        .route(
            "/process.Process/Connect",
            axum::routing::post(pty::connect),
        )
        .route(
            "/process.Process/SendSignal",
            axum::routing::post(pty::send_signal),
        )
        .route(
            "/process.Process/SendInput",
            axum::routing::post(pty::send_input),
        )
        .route(
            "/process.Process/StreamInput",
            axum::routing::post(process::stream_input),
        )
        .route(
            "/process.Process/CloseStdin",
            axum::routing::post(process::close_stdin),
        )
        .route("/process.Process/Update", axum::routing::post(pty::update))
        .route("/files", get(files::get).post(files::post))
        .route("/files/compose", axum::routing::post(files::compose))
        .route(
            "/filesystem.Filesystem/ListDir",
            axum::routing::post(filesystem::list_dir),
        )
        .route(
            "/filesystem.Filesystem/Stat",
            axum::routing::post(filesystem::stat),
        )
        .route(
            "/filesystem.Filesystem/Remove",
            axum::routing::post(filesystem::remove),
        )
        .route(
            "/filesystem.Filesystem/Move",
            axum::routing::post(filesystem::move_entry),
        )
        .route(
            "/filesystem.Filesystem/MakeDir",
            axum::routing::post(filesystem::make_dir),
        )
        .route(
            "/filesystem.Filesystem/WatchDir",
            axum::routing::post(watch::watch_dir),
        )
        .route(
            "/filesystem.Filesystem/CreateWatcher",
            axum::routing::post(watch::create_watcher),
        )
        .route(
            "/filesystem.Filesystem/GetWatcherEvents",
            axum::routing::post(watch::get_watcher_events),
        )
        .route(
            "/filesystem.Filesystem/RemoveWatcher",
            axum::routing::post(watch::remove_watcher),
        )
        .with_state(state)
        .layer(axum::middleware::from_fn(codec_guard))
        .layer(axum::middleware::from_fn(auth::layer))
        .layer(axum::middleware::from_fn(request_log))
        .layer(axum::middleware::from_fn(cors::layer));
    axum::serve(listener, app).await?;

    Ok(())
}

#[cfg(test)]
mod tests {
    use super::{
        configured_log_format, configured_log_level, filtered_args, normalized_args,
        protobuf_codec_requested, request_id, version_line, AppState, LogFormat, StatusResponse,
        ENVD_GENERATION, REQUEST_ID_HEADER,
    };
    use axum::http::{HeaderMap, HeaderValue};
    use std::{sync::Arc, time::Instant};

    #[test]
    fn legacy_flags_are_normalized() {
        let args = normalized_args(
            [
                "cube-envd",
                "-port",
                "59999",
                "-isnotfc",
                "-version",
                "-commit",
            ]
            .into_iter()
            .map(str::to_owned),
        );
        assert_eq!(
            args,
            vec![
                "cube-envd",
                "--port",
                "59999",
                "--isnotfc",
                "--version",
                "--commit"
            ]
        );
    }

    #[test]
    fn status_response_uses_stable_contract() {
        let state = Arc::new(AppState {
            port: 49983,
            started_at: Instant::now(),
        });
        let response = StatusResponse {
            service: "cube-envd",
            ready: true,
            version: ENVD_GENERATION,
            commit: env!("GIT_SHORT_SHA"),
            port: state.port,
            uptime_seconds: state.started_at.elapsed().as_secs(),
        };
        let value = serde_json::to_value(response).expect("status response serializes");

        assert_eq!(value["service"], "cube-envd");
        assert_eq!(value["ready"], true);
        assert_eq!(value["version"], ENVD_GENERATION);
        assert!(value["commit"].is_string());
        assert_eq!(value["port"], 49983);
        assert!(value["uptimeSeconds"].is_u64());
        assert!(value.get("uptime_seconds").is_none());
    }

    #[test]
    fn logging_configuration_has_safe_defaults() {
        assert_eq!(
            configured_log_level(None),
            tracing_subscriber::filter::LevelFilter::INFO
        );
        assert_eq!(
            configured_log_level(Some("invalid")),
            tracing_subscriber::filter::LevelFilter::INFO
        );
        assert_eq!(
            configured_log_level(Some("warn")),
            tracing_subscriber::filter::LevelFilter::WARN
        );
        assert_eq!(configured_log_format(None), LogFormat::Pretty);
        assert_eq!(configured_log_format(Some("json")), LogFormat::Json);
        assert_eq!(configured_log_format(Some("unknown")), LogFormat::Pretty);
    }

    #[test]
    fn request_id_preserves_safe_client_value() {
        let mut headers = HeaderMap::new();
        headers.insert(REQUEST_ID_HEADER, HeaderValue::from_static("request-123"));

        assert_eq!(request_id(&headers), "request-123");
    }

    #[test]
    fn request_id_replaces_invalid_or_missing_values() {
        for value in ["", "contains spaces", "contains/slash"] {
            let mut headers = HeaderMap::new();
            headers.insert(
                REQUEST_ID_HEADER,
                HeaderValue::from_str(value).expect("test header is valid"),
            );
            assert!(request_id(&headers).starts_with("cube-envd-"));
        }

        let generated = request_id(&HeaderMap::new());
        assert!(generated.starts_with("cube-envd-"));
    }

    #[test]
    fn request_id_rejects_overlong_value() {
        let mut headers = HeaderMap::new();
        let long = "a".repeat(129);
        headers.insert(REQUEST_ID_HEADER, HeaderValue::from_str(&long).unwrap());
        let id = request_id(&headers);
        assert!(id.starts_with("cube-envd-"));
        assert_ne!(id, long);
    }

    #[test]
    fn version_line_reports_the_emulated_generation() {
        let line = version_line();
        assert_eq!(line, format!("cube-envd {ENVD_GENERATION}"));
        // Keeps the `cube-envd ` prefix the base-image smoke test asserts.
        assert!(line.starts_with("cube-envd "));
        // Contains a semver so Cubelet's `envd --version` probe extracts it.
        assert!(
            line.split_whitespace().any(|token| {
                token.split('.').count() == 3
                    && token.split('.').all(|part| {
                        !part.is_empty() && part.bytes().all(|byte| byte.is_ascii_digit())
                    })
            }),
            "expected a semver in {line:?}"
        );
    }

    #[test]
    fn legacy_port_equals_form_is_normalized() {
        let args = normalized_args(["-port=1234"].into_iter().map(str::to_owned));
        assert_eq!(args, vec!["--port=1234"]);
    }

    #[test]
    fn unknown_flags_are_dropped_with_their_values() {
        let args = filtered_args(
            [
                "cube-envd",
                "--port",
                "39999",
                "--isnotfc",
                "--unknown",
                "value",
                "--other=1",
                "stray",
                "--version",
            ]
            .into_iter()
            .map(str::to_owned),
        );
        assert_eq!(
            args,
            vec!["cube-envd", "--port", "39999", "--isnotfc", "--version"]
        );
    }

    #[test]
    fn protobuf_codec_is_detected_case_insensitively() {
        for value in [
            "application/proto",
            "application/connect+proto",
            "application/grpc",
            "application/grpc+proto",
            "application/grpc-web",
            "application/grpc-web+proto",
            "application/proto; charset=utf-8",
            "Application/Proto",
        ] {
            let mut headers = HeaderMap::new();
            headers.insert(
                axum::http::header::CONTENT_TYPE,
                HeaderValue::from_str(value).unwrap(),
            );
            assert!(
                protobuf_codec_requested(&headers),
                "expected proto: {value}"
            );
        }
    }

    #[test]
    fn json_and_missing_content_types_are_not_protobuf() {
        for value in [
            None,
            Some("application/json"),
            Some("application/connect+json"),
        ] {
            let mut headers = HeaderMap::new();
            if let Some(value) = value {
                headers.insert(
                    axum::http::header::CONTENT_TYPE,
                    HeaderValue::from_str(value).unwrap(),
                );
            }
            assert!(
                !protobuf_codec_requested(&headers),
                "unexpected proto: {value:?}"
            );
        }
    }
}
