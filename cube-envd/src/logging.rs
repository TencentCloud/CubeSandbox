use tracing_subscriber::EnvFilter;

/// 从可选的 RUST_LOG 指令创建过滤器，缺失或无效时回退到 info。
pub fn filter_from_directive(directive: Option<&str>) -> EnvFilter {
    directive
        .and_then(|directive| directive.parse::<EnvFilter>().ok())
        .unwrap_or_else(|| EnvFilter::new("info"))
}

/// 初始化全局 JSON 结构化日志，并避免重复初始化导致服务启动失败。
pub fn init() {
    let directive = std::env::var("RUST_LOG").ok();
    let filter = filter_from_directive(directive.as_deref());
    let _ = tracing_subscriber::fmt()
        .json()
        .with_env_filter(filter)
        .with_current_span(false)
        .with_span_list(false)
        .try_init();
}
