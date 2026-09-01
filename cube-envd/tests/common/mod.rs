use base64::{engine::general_purpose::STANDARD, Engine};
use nix::unistd::{getuid, User};

/// 返回运行测试的实际本地用户名。
#[allow(dead_code)]
pub fn current_username() -> String {
    User::from_uid(getuid())
        .expect("look up current local user")
        .map(|user| user.name)
        .expect("current uid has a local user entry")
}

/// 生成当前本地用户的 Basic 用户选择认证头。
#[allow(dead_code)]
pub fn basic_auth_header() -> String {
    format!(
        "Basic {}",
        STANDARD.encode(format!("{}:", current_username()))
    )
}
