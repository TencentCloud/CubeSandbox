use std::{ffi::OsString, net::SocketAddr};

use clap::Parser;

/// 优先使用构建注入的发布版本，否则回退到 Cargo 包版本。
const VERSION: &str = match option_env!("CUBE_ENVD_VERSION") {
    Some(version) => version,
    None => env!("CARGO_PKG_VERSION"),
};
/// 优先使用构建注入的提交哈希，否则标记为未知。
const COMMIT: &str = match option_env!("CUBE_ENVD_COMMIT") {
    Some(commit) => commit,
    None => "unknown",
};

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
    axum::serve(listener, cube_envd::app::router_with_state(state))
        .with_graceful_shutdown(async move {
            shutdown_signal().await;
            shutdown_signal_state.begin_shutdown();
        })
        .await?;
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
