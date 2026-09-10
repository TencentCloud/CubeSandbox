use std::fs;

use axum::{
    body::Body,
    http::{header::CONTENT_TYPE, Request, StatusCode},
};
use cube_envd::app::router;
use http_body_util::BodyExt;
use serde_json::{json, Value};
use tempfile::tempdir;
use tower::ServiceExt;

mod common;

// 发送一元文件系统 RPC 并解码其状态码和 JSON 响应体。
async fn rpc(app: axum::Router, method: &str, payload: Value) -> (StatusCode, Value) {
    let response = app
        .oneshot(
            Request::post(format!("/filesystem.Filesystem/{method}"))
                .header(CONTENT_TYPE, "application/json")
                .header("Connect-Protocol-Version", "1")
                .header("Authorization", common::basic_auth_header())
                .body(Body::from(payload.to_string()))
                .unwrap(),
        )
        .await
        .unwrap();
    let status = response.status();
    let body = response.into_body().collect().await.unwrap().to_bytes();
    let json = if body.is_empty() {
        json!({})
    } else {
        serde_json::from_slice(&body).unwrap()
    };
    (status, json)
}

// 验证文件系统 RPC 能串联完成 stat、列表、移动、建目录和删除。
#[tokio::test]
async fn filesystem_rpc_creates_lists_moves_stats_and_removes_entries() {
    let directory = tempdir().unwrap();
    let source = directory.path().join("nested/source.txt");
    let destination = directory.path().join("nested/moved.txt");
    let app = router();
    fs::create_dir_all(source.parent().unwrap()).unwrap();
    fs::write(&source, b"cube-envd").unwrap();

    let (status, body) = rpc(app.clone(), "Stat", json!({"path": source})).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(body["entry"]["type"], "FILE_TYPE_FILE");
    assert_eq!(body["entry"]["size"], "9");

    let (status, body) = rpc(
        app.clone(),
        "ListDir",
        json!({"path": directory.path(), "depth": 2}),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert!(body["entries"]
        .as_array()
        .unwrap()
        .iter()
        .any(|entry| entry["name"] == "source.txt"));

    let (status, body) = rpc(
        app.clone(),
        "Move",
        json!({"source": source, "destination": destination}),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(body["entry"]["name"], "moved.txt");
    assert!(destination.exists());

    let new_dir = directory.path().join("another/deep/dir");
    let (status, body) = rpc(app.clone(), "MakeDir", json!({"path": new_dir})).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(body["entry"]["type"], "FILE_TYPE_DIRECTORY");

    let (status, body) = rpc(app, "Remove", json!({"path": directory.path()})).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(body, json!({}));
    assert!(!directory.path().exists());
}

// 验证删除不存在的路径是幂等成功，与上游 os.RemoveAll 一致。
#[tokio::test]
async fn filesystem_remove_is_idempotent_for_missing_paths() {
    let directory = tempdir().unwrap();
    let missing = directory.path().join("never-existed");

    let (status, body) = rpc(router(), "Remove", json!({"path": missing})).await;

    assert_eq!(status, StatusCode::OK);
    assert_eq!(body, json!({}));
}

// 验证已存在目录返回 409，已存在非目录返回 400，与上游一致。
#[tokio::test]
async fn filesystem_make_dir_distinguishes_conflict_from_invalid_path() {
    let directory = tempdir().unwrap();
    let existing_directory = directory.path().join("exists");
    let existing_file = directory.path().join("file.txt");
    fs::create_dir(&existing_directory).unwrap();
    fs::write(&existing_file, b"x").unwrap();

    let (status, body) = rpc(router(), "MakeDir", json!({"path": existing_directory})).await;
    assert_eq!(status, StatusCode::CONFLICT, "body: {body}");
    assert_eq!(body["code"], "already_exists");

    let (status, body) = rpc(router(), "MakeDir", json!({"path": existing_file})).await;
    assert_eq!(status, StatusCode::BAD_REQUEST, "body: {body}");
    assert_eq!(body["code"], "invalid_argument");
}

// 验证空路径与上游一致地解析为请求用户的主目录。
#[tokio::test]
async fn filesystem_stat_resolves_the_empty_path_to_the_user_home() {
    let current = nix::unistd::User::from_uid(nix::unistd::getuid())
        .expect("look up current local user")
        .expect("current uid has a passwd entry");

    let (status, body) = rpc(router(), "Stat", json!({"path": ""})).await;

    assert_eq!(status, StatusCode::OK, "body: {body}");
    assert_eq!(body["entry"]["type"], "FILE_TYPE_DIRECTORY");
    assert_eq!(body["entry"]["path"], current.dir.display().to_string());
}

// 验证 RPC 请求体中的未知字段被忽略，而不是 400（上游 protojson DiscardUnknown）。
#[tokio::test]
async fn filesystem_rpc_ignores_unknown_json_fields() {
    let directory = tempdir().unwrap();

    let (status, body) = rpc(
        router(),
        "ListDir",
        json!({"path": directory.path(), "depth": 1, "futureField": {"nested": true}}),
    )
    .await;

    assert_eq!(status, StatusCode::OK, "body: {body}");
    // 空 repeated 字段是零值，proto3 JSON 会省略。
    assert!(
        body.get("entries").is_none(),
        "empty entry list must be omitted: {body}"
    );
}

// 验证递归删除拒绝文件系统根目录，避免一句 RPC 清空 guest 文件系统。
#[tokio::test]
async fn filesystem_remove_refuses_the_filesystem_root() {
    let (status, body) = rpc(router(), "Remove", json!({"path": "/"})).await;

    assert_eq!(status, StatusCode::BAD_REQUEST, "body: {body}");
    assert_eq!(body["code"], "invalid_argument");
    assert!(
        std::path::Path::new("/etc").exists(),
        "the guest filesystem must be untouched"
    );
}

// 验证拒绝的是根/挂载点，普通目录仍可递归删除。
#[tokio::test]
async fn filesystem_remove_still_deletes_ordinary_directories() {
    let directory = tempdir().unwrap();
    let nested = directory.path().join("nested/deep");
    fs::create_dir_all(&nested).unwrap();
    fs::write(nested.join("file.txt"), b"x").unwrap();

    let (status, body) = rpc(router(), "Remove", json!({"path": nested})).await;

    assert_eq!(status, StatusCode::OK, "body: {body}");
    assert!(!nested.exists());
    assert!(directory.path().exists());
}

// 验证文件系统 RPC 会拒绝未知 Basic 用户。
#[tokio::test]
async fn filesystem_rpc_rejects_unknown_basic_users() {
    let response = router()
        .oneshot(
            Request::post("/filesystem.Filesystem/Stat")
                .header(CONTENT_TYPE, "application/json")
                .header("Connect-Protocol-Version", "1")
                .header("Authorization", "Basic bm90LWEtdXNlcjo=")
                .body(Body::from(r#"{"path":"/tmp"}"#))
                .unwrap(),
        )
        .await
        .unwrap();

    assert_eq!(response.status(), StatusCode::UNAUTHORIZED);
}

// 验证测试环境提供了用于文件系统 RPC 的用户名。
#[test]
fn test_user_name_is_available_for_filesystem_requests() {
    assert!(!common::current_username().is_empty());
}

// 验证 Filesystem.Stat 对符号链接上报目标的类型、绝对解析路径与 lstat 权限串。
#[cfg(unix)]
#[tokio::test]
async fn filesystem_stat_reports_symbolic_link_metadata() {
    let directory = tempdir().unwrap();
    let target = directory.path().join("target.txt");
    let link = directory.path().join("link.txt");
    fs::write(&target, b"target").unwrap();
    std::os::unix::fs::symlink(&target, &link).unwrap();

    let (status, body) = rpc(router(), "Stat", json!({"path": link})).await;

    assert_eq!(status, StatusCode::OK);
    // 与上游一致：type/mode 取自目标，permissions 取自 lstat 且含类型前缀字符。
    assert_eq!(body["entry"]["type"], "FILE_TYPE_FILE");
    assert_eq!(body["entry"]["symlinkTarget"], target.display().to_string());
    assert_eq!(body["entry"]["permissions"], "Lrwxrwxrwx");
    assert_eq!(body["entry"]["mode"], 0o644);
}

// 验证指向目录的符号链接按目录类型上报，使 SDK 的 IsDir 判定与上游一致。
#[cfg(unix)]
#[tokio::test]
async fn filesystem_stat_reports_a_directory_symlink_as_a_directory() {
    let directory = tempdir().unwrap();
    let target = directory.path().join("target-dir");
    let link = directory.path().join("link-dir");
    fs::create_dir(&target).unwrap();
    std::os::unix::fs::symlink(&target, &link).unwrap();

    let (status, body) = rpc(router(), "Stat", json!({"path": link})).await;

    assert_eq!(status, StatusCode::OK);
    assert_eq!(body["entry"]["type"], "FILE_TYPE_DIRECTORY");
}

// 验证悬空符号链接上报未指定类型、mode 0，且 symlinkTarget 回退为原路径。
#[cfg(unix)]
#[tokio::test]
async fn filesystem_stat_reports_a_dangling_symlink_as_unspecified() {
    let directory = tempdir().unwrap();
    let link = directory.path().join("dangling");
    std::os::unix::fs::symlink(directory.path().join("missing"), &link).unwrap();

    let (status, body) = rpc(router(), "Stat", json!({"path": link})).await;

    assert_eq!(status, StatusCode::OK);
    // FILE_TYPE_UNSPECIFIED 与 mode 0 都是零值，proto3 JSON 会省略它们；解析失败时
    // symlinkTarget 与上游一样回退为传入路径。
    assert!(
        body["entry"].get("type").is_none(),
        "unspecified type must be omitted: {body}"
    );
    assert!(
        body["entry"].get("mode").is_none(),
        "zero mode must be omitted: {body}"
    );
    assert_eq!(body["entry"]["permissions"], "Lrwxrwxrwx");
    assert_eq!(body["entry"]["symlinkTarget"], link.display().to_string());
}

// 验证普通文件与目录的权限串带类型前缀字符且不含 suid/sticky 位。
#[cfg(unix)]
#[tokio::test]
async fn filesystem_stat_renders_permissions_like_the_reference_envd() {
    use std::os::unix::fs::PermissionsExt;

    let directory = tempdir().unwrap();
    let file = directory.path().join("file.txt");
    fs::write(&file, b"x").unwrap();
    fs::set_permissions(&file, fs::Permissions::from_mode(0o640)).unwrap();

    let (status, body) = rpc(router(), "Stat", json!({"path": file})).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(body["entry"]["permissions"], "-rw-r-----");
    assert_eq!(body["entry"]["type"], "FILE_TYPE_FILE");

    let (status, body) = rpc(router(), "Stat", json!({"path": directory.path()})).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(body["entry"]["type"], "FILE_TYPE_DIRECTORY");
    let permissions = body["entry"]["permissions"].as_str().unwrap();
    assert!(permissions.starts_with('d'), "permissions: {permissions}");
    assert_eq!(permissions.len(), 10, "permissions: {permissions}");

    // 数值 mode 只保留 0o777：setuid 只体现在 permissions 的前缀字符上。
    fs::set_permissions(&file, fs::Permissions::from_mode(0o4755)).unwrap();
    let (status, body) = rpc(router(), "Stat", json!({"path": file})).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(body["entry"]["mode"], 0o755);
    assert_eq!(body["entry"]["permissions"], "urwxr-xr-x");
}
