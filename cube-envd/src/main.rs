use std::{ffi::OsString, net::SocketAddr, sync::Arc, time::Duration};

use clap::Parser;
use hyper_util::{
    rt::{TokioExecutor, TokioIo, TokioTimer},
    server::conn::auto::Builder as ConnectionBuilder,
};

/// 解析 `-version` 输出的版本号。
///
/// 默认值是源码常量（见 [`cube_envd::version`]，与上游 e2b envd 的 `pkg/version.go`
/// 同构，是版本的唯一事实源），因此任何构建渠道都得到同一个真 semver。
/// `CUBE_ENVD_VERSION` 仅作为**显式覆盖**保留（发布渠道如有需要可 stamp），空串按未设置
/// 处理，避免把空值当成版本输出。
const fn resolve_version(injected: Option<&str>) -> &str {
    match injected {
        Some(version) if !version.is_empty() => version,
        _ => cube_envd::version::CUBE_ENVD_VERSION,
    }
}

/// `-version` 与启动日志使用的版本号。
const VERSION: &str = resolve_version(option_env!("CUBE_ENVD_VERSION"));
/// 优先使用构建注入的提交哈希，否则标记为未知。
const COMMIT: &str = match option_env!("CUBE_ENVD_COMMIT") {
    Some(commit) => commit,
    None => "unknown",
};

/// 读取请求头的上限时间，取值与参考实现 envd 的 IdleTimeout 一致。
///
/// hyper 的这个超时同时覆盖"首个请求头"与"keep-alive 连接上的下一个请求头"，因此它
/// 也是空闲连接的上限：既保证慢速请求不会无限占用任务与 fd，又不会比上游更早掐断
/// 客户端连接池里的空闲连接。
const REQUEST_HEADER_TIMEOUT: Duration = Duration::from_secs(640);
/// 同时存在的客户端连接数上限，防止连接洪泛耗尽 guest 共享的 fd 预算。
const MAX_CONNECTIONS: usize = 1024;
/// 停机时为在途请求预留的排空时间，超时后强制回收受管进程。
///
/// 流式 RPC 已由取消令牌结束，这里主要留给短请求收尾；该值同时决定"envd 收到
/// SIGTERM 到子进程被终止"的最坏延迟。
const SHUTDOWN_DRAIN_TIMEOUT: Duration = Duration::from_secs(2);

#[derive(Parser)]
#[command(name = "envd", disable_version_flag = true)]
/// 定义兼容既有 envd 调用方式的命令行参数。
struct Cli {
    /// 指定 HTTP 服务监听端口。
    #[arg(long, default_value_t = 49_983)]
    port: u16,
    /// 保留 Firecracker 兼容参数，但当前不改变行为。
    #[arg(long)]
    isnotfc: bool,
    /// 仅输出版本并退出。
    #[arg(long)]
    version: bool,
    /// 仅输出提交哈希并退出。
    #[arg(long)]
    commit: bool,
}

#[tokio::main]
/// 启动 HTTP 服务，并在收到终止信号后优雅回收受管进程。
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let cli = Cli::parse_from(normalize_compatibility_flags(std::env::args_os()));
    if cli.version {
        println!("{VERSION}");
        return Ok(());
    }
    if cli.commit {
        println!("{COMMIT}");
        return Ok(());
    }

    cube_envd::logging::init();
    let address = SocketAddr::from(([0, 0, 0, 0], cli.port));
    let listener = tokio::net::TcpListener::bind(address).await?;
    tracing::info!(
        port = cli.port,
        version = VERSION,
        commit = COMMIT,
        "cube-envd is listening"
    );
    let state = cube_envd::app::AppState::default();
    let shutdown_state = state.clone();
    let shutdown_signal_state = shutdown_state.clone();
    let router = cube_envd::app::router_with_state(state);

    // 与参考实现一样不为读/写设置整体超时（流式 RPC 可能长期存活），但为"读取请求头"
    // 与 keep-alive 空闲设上限，并限制并发连接数：否则慢速请求或连接洪泛会持续占用
    // 任务与 fd，而 guest 的 fd 预算是全局共享的。
    let mut builder = ConnectionBuilder::new(TokioExecutor::new());
    builder
        .http1()
        .timer(TokioTimer::new())
        .header_read_timeout(REQUEST_HEADER_TIMEOUT)
        .keep_alive(true);
    let builder = Arc::new(builder);
    let connections = Arc::new(tokio::sync::Semaphore::new(MAX_CONNECTIONS));
    let (stop_accepting, mut stopping) = tokio::sync::watch::channel(false);

    let server = {
        let listener = listener;
        let router = router.clone();
        async move {
            loop {
                let permit = match Arc::clone(&connections).acquire_owned().await {
                    Ok(permit) => permit,
                    Err(_) => return,
                };
                let accepted = tokio::select! {
                    accepted = listener.accept() => accepted,
                    _ = stopping.changed() => return,
                };
                let (stream, _) = match accepted {
                    Ok(connection) => connection,
                    // 单个连接的 accept 失败不应终止整个服务。
                    Err(error) => {
                        tracing::warn!(error = %error, "accept failed");
                        continue;
                    }
                };
                let builder = Arc::clone(&builder);
                let router = router.clone();
                let mut shutdown = stopping.clone();
                tokio::spawn(async move {
                    let _permit = permit;
                    let service = hyper::service::service_fn(move |request| {
                        let router = router.clone();
                        async move { tower::ServiceExt::oneshot(router, request).await }
                    });
                    let connection =
                        builder.serve_connection_with_upgrades(TokioIo::new(stream), service);
                    tokio::pin!(connection);
                    tokio::select! {
                        result = &mut connection => {
                            if let Err(error) = result {
                                tracing::debug!(error = %error, "connection failed");
                            }
                        }
                        _ = shutdown.changed() => {
                            // 停机时给在途请求一个排空窗口，超时即强制结束该连接。
                            let _ = tokio::time::timeout(SHUTDOWN_DRAIN_TIMEOUT, &mut connection).await;
                        }
                    }
                });
            }
        }
    };

    let accept_handle = tokio::spawn(server);
    shutdown_signal().await;
    shutdown_signal_state.begin_shutdown();
    let _ = stop_accepting.send(true);
    // 停机总预算：先给在途请求一个排空窗口，再无条件回收受管进程——否则一个挂住的
    // 客户端会让子进程在 envd 退出后继续存活。
    let _ = tokio::time::timeout(SHUTDOWN_DRAIN_TIMEOUT, accept_handle).await;
    shutdown_state.shutdown_processes().await;
    tracing::info!("cube-envd shut down");

    Ok(())
}

/// 将历史单横线参数规范化为 clap 接受的双横线形式。
fn normalize_compatibility_flags(arguments: impl IntoIterator<Item = OsString>) -> Vec<OsString> {
    arguments
        .into_iter()
        .map(|argument| match argument.to_str() {
            Some("-port") => OsString::from("--port"),
            Some("-isnotfc") => OsString::from("--isnotfc"),
            Some("-version") => OsString::from("--version"),
            Some("-commit") => OsString::from("--commit"),
            _ => argument,
        })
        .collect()
}

/// 等待 Ctrl-C 或 Unix SIGTERM 以触发优雅关闭。
async fn shutdown_signal() {
    #[cfg(unix)]
    {
        let mut terminate =
            tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())
                .expect("install SIGTERM handler");
        tokio::select! {
            _ = tokio::signal::ctrl_c() => {}
            _ = terminate.recv() => {}
        }
    }
    #[cfg(not(unix))]
    {
        let _ = tokio::signal::ctrl_c().await;
    }
}
