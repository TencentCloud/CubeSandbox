use std::path::PathBuf;

use axum::http::{header::AUTHORIZATION, HeaderMap};
use cube_envd::{
    auth::{basic_username, LocalUser},
    paths::resolve_path,
};

// 构造用于路径和认证测试的固定本地用户。
fn user() -> LocalUser {
    LocalUser {
        name: "alice".into(),
        uid: 1000,
        gid: 1000,
        groups: vec![1000],
        home: PathBuf::from("/home/alice"),
    }
}

// 验证缺失 Basic 认证头时回退为 root 用户。
#[test]
fn missing_basic_auth_uses_root() {
    assert_eq!(basic_username(&HeaderMap::new()).unwrap(), "root");
}

// 验证 Basic 认证只使用用户名而忽略密码内容。
#[test]
fn basic_auth_uses_the_username_and_ignores_the_password() {
    let mut headers = HeaderMap::new();
    headers.insert(AUTHORIZATION, "Basic YWxpY2U6c2VjcmV0".parse().unwrap());

    assert_eq!(basic_username(&headers).unwrap(), "alice");
}

// 验证格式错误的 Basic 认证头会被拒绝。
#[test]
fn invalid_basic_auth_is_rejected() {
    let mut headers = HeaderMap::new();
    headers.insert(AUTHORIZATION, "Basic not-base64".parse().unwrap());

    assert!(basic_username(&headers).is_err());
}

// 验证相对路径和 ~/ 路径都会在用户主目录下解析。
#[test]
fn paths_resolve_relative_and_tilde_forms_under_the_user_home() {
    assert_eq!(
        resolve_path("workspace/a", &user()).unwrap(),
        PathBuf::from("/home/alice/workspace/a")
    );
    assert_eq!(
        resolve_path("~/workspace/a", &user()).unwrap(),
        PathBuf::from("/home/alice/workspace/a")
    );
    assert_eq!(
        resolve_path("/tmp/a", &user()).unwrap(),
        PathBuf::from("/tmp/a")
    );
}

// 验证不能展开其他用户主目录，且保留相对路径中的父目录段。
#[test]
fn paths_reject_another_users_home_but_preserve_parent_components() {
    assert!(resolve_path("~bob/private", &user()).is_err());
    assert_eq!(
        resolve_path("../shared", &user()).unwrap(),
        PathBuf::from("/home/alice/../shared")
    );
}
