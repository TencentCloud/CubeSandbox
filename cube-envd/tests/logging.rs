use cube_envd::logging;

// 验证未设置 RUST_LOG 时结构化日志使用 info 默认级别。
#[test]
fn logging_uses_info_when_no_filter_is_configured() {
    assert_eq!(logging::filter_from_directive(None).to_string(), "info");
}
