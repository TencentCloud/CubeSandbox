mod common;

use std::{process::Command, time::Duration};

use cube_envd::connect::encode_frame;
use nix::{
    sys::signal::{kill, Signal},
    unistd::Pid,
};
use serde_json::json;
use tempfile::tempdir;
use tokio::io::{AsyncReadExt, AsyncWriteExt};

// 验证兼容版本和提交参数都会输出内容并正常退出。
#[test]
fn version_and_commit_flags_print_and_exit() {
    let binary = env!("CARGO_BIN_EXE_cube-envd");
    let version = Command::new(binary).arg("-version").output().unwrap();
    assert!(version.status.success());
    assert!(!String::from_utf8(version.stdout).unwrap().trim().is_empty());

    let commit = Command::new(binary).arg("-commit").output().unwrap();
    assert!(commit.status.success());
    assert!(!String::from_utf8(commit.stdout).unwrap().trim().is_empty());
}

// 验证二进制能在指定端口启动并返回健康检查状态。
#[tokio::test]
async fn binary_serves_health_on_the_requested_port() {
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let port = listener.local_addr().unwrap().port();
    drop(listener);
    let binary = env!("CARGO_BIN_EXE_cube-envd");
    let mut child = tokio::process::Command::new(binary)
        .args(["-port", &port.to_string(), "-isnotfc"])
        .kill_on_drop(true)
        .spawn()
        .unwrap();

    let mut response = None;
    for _ in 0..20 {
        if let Ok(mut stream) = tokio::net::TcpStream::connect(("127.0.0.1", port)).await {
            stream
                .write_all(b"GET /health HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")
                .await
                .unwrap();
            let mut bytes = Vec::new();
            stream.read_to_end(&mut bytes).await.unwrap();
            response = Some(String::from_utf8(bytes).unwrap());
            break;
        }
        tokio::time::sleep(Duration::from_millis(25)).await;
    }

    child.kill().await.unwrap();
    assert!(response.unwrap().starts_with("HTTP/1.1 204"));
}

// 验证向 envd 发送 SIGTERM 会终止并回收其管理的进程组。
#[tokio::test]
async fn sigterm_terminates_processes_started_by_envd() {
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let port = listener.local_addr().unwrap().port();
    drop(listener);
    let binary = env!("CARGO_BIN_EXE_cube-envd");
    let mut envd = tokio::process::Command::new(binary)
        .args(["-port", &port.to_string(), "-isnotfc"])
        .kill_on_drop(true)
        .spawn()
        .unwrap();
    let frame = encode_frame(
        0,
        json!({
            "process": {"cmd": "/bin/sleep", "args": ["30"], "envs": {}},
            "stdin": false
        })
        .to_string()
        .as_bytes(),
    )
    .unwrap();

    let mut response = Vec::new();
    for _ in 0..20 {
        if let Ok(mut stream) = tokio::net::TcpStream::connect(("127.0.0.1", port)).await {
            let request = format!(
                "POST /process.Process/Start HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/connect+json\r\nConnect-Protocol-Version: 1\r\nContent-Length: {}\r\n\r\n",
                frame.len()
            );
            stream.write_all(request.as_bytes()).await.unwrap();
            stream.write_all(&frame).await.unwrap();
            for _ in 0..20 {
                let mut chunk = [0_u8; 4096];
                let read =
                    tokio::time::timeout(Duration::from_millis(100), stream.read(&mut chunk))
                        .await
                        .unwrap()
                        .unwrap();
                response.extend_from_slice(&chunk[..read]);
                if String::from_utf8_lossy(&response).contains("\"pid\":") {
                    break;
                }
            }
            break;
        }
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
    let response_text = String::from_utf8_lossy(&response);
    assert!(response_text.starts_with("HTTP/1.1 200"));
    let pid = started_pid(&response_text).expect("Start response must contain a PID");

    kill(Pid::from_raw(envd.id().unwrap() as i32), Signal::SIGTERM).unwrap();
    tokio::time::timeout(Duration::from_secs(5), envd.wait())
        .await
        .expect("envd exits after SIGTERM")
        .unwrap();

    let terminated =
        kill(Pid::from_raw(pid as i32), None).is_err_and(|error| error == nix::errno::Errno::ESRCH);
    if !terminated {
        let _ = kill(Pid::from_raw(-(pid as i32)), Signal::SIGKILL);
    }
    assert!(
        terminated,
        "envd SIGTERM must terminate and reap managed process groups"
    );
}

// 验证活跃 WatchDir 流存在时 SIGTERM 仍能让服务关闭。
#[tokio::test]
async fn sigterm_closes_an_active_watch_stream_before_server_shutdown() {
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let port = listener.local_addr().unwrap().port();
    drop(listener);
    let binary = env!("CARGO_BIN_EXE_cube-envd");
    let mut envd = tokio::process::Command::new(binary)
        .args(["-port", &port.to_string(), "-isnotfc"])
        .kill_on_drop(true)
        .spawn()
        .unwrap();
    let directory = tempdir().unwrap();
    let frame = encode_frame(0, json!({"path": directory.path()}).to_string().as_bytes()).unwrap();
    let mut stream =
        wait_for_streaming_request(port, "/filesystem.Filesystem/WatchDir", frame).await;
    let mut first = [0_u8; 4096];
    let read = tokio::time::timeout(Duration::from_secs(1), stream.read(&mut first))
        .await
        .expect("WatchDir response start frame")
        .unwrap();
    assert!(String::from_utf8_lossy(&first[..read]).contains("HTTP/1.1 200"));

    kill(Pid::from_raw(envd.id().unwrap() as i32), Signal::SIGTERM).unwrap();
    tokio::time::timeout(Duration::from_secs(3), envd.wait())
        .await
        .expect("envd exits while WatchDir stream is active")
        .unwrap();
}

// 轮询连接服务并发送一个 Connect 流式请求。
async fn wait_for_streaming_request(
    port: u16,
    endpoint: &str,
    frame: Vec<u8>,
) -> tokio::net::TcpStream {
    let authorization = common::basic_auth_header();
    for _ in 0..20 {
        if let Ok(mut stream) = tokio::net::TcpStream::connect(("127.0.0.1", port)).await {
            let request = format!(
                "POST {endpoint} HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/connect+json\r\nConnect-Protocol-Version: 1\r\nAuthorization: {authorization}\r\nContent-Length: {}\r\n\r\n",
                frame.len()
            );
            stream.write_all(request.as_bytes()).await.unwrap();
            stream.write_all(&frame).await.unwrap();
            return stream;
        }
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
    panic!("envd did not accept streaming request");
}

// 从原始 HTTP 响应文本中提取 Start 事件的 PID。
fn started_pid(response: &str) -> Option<u32> {
    let response = response.split_once("\"pid\":")?.1;
    let digits: String = response.chars().take_while(char::is_ascii_digit).collect();
    digits.parse().ok()
}
