// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use std::ffi::CString;
use std::fs::{File, OpenOptions};
use std::io::Write;
use std::os::unix::fs::{symlink, OpenOptionsExt};
use std::path::Path;
use std::sync::Arc;
use std::time::Duration;

use cube_envd::runtime::UserDatabase;
use cube_envd::server::{LifecycleState, ServerPhase};
use serde_json::{json, Value};

async fn server(passwd: &Path) -> (u16, tokio::task::JoinHandle<()>) {
    let lifecycle = Arc::new(LifecycleState::new());
    assert!(lifecycle.try_transition(ServerPhase::Listening));
    assert!(lifecycle.try_transition(ServerPhase::Ready));
    let mut state = cube_envd::transport::new_app_state(lifecycle);
    Arc::get_mut(&mut state).unwrap().users = UserDatabase::from_passwd_file(passwd);
    let router = cube_envd::transport::build_router(state);
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let port = listener.local_addr().unwrap().port();
    let task = tokio::spawn(async move { axum::serve(listener, router).await.unwrap() });
    (port, task)
}

async fn init(port: u16, body: Value) {
    let response = reqwest::Client::new()
        .post(format!("http://127.0.0.1:{port}/init"))
        .timeout(Duration::from_secs(5))
        .json(&body)
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 204);
}

async fn rpc(port: u16, method: &str, body: Value, user: Option<&str>) -> Value {
    let request = reqwest::Client::new()
        .post(format!(
            "http://127.0.0.1:{port}/filesystem.Filesystem/{method}"
        ))
        .timeout(Duration::from_secs(5))
        .header("connect-protocol-version", "1")
        .json(&body);
    let request = if let Some(user) = user {
        request.basic_auth(user, Some(""))
    } else {
        request
    };
    request.send().await.unwrap().json().await.unwrap()
}

fn passwd(home: &Path, other: &Path) -> String {
    format!(
        "root:x:0:0::{}:/bin/sh\nalice:x:12345:12345::{}:/bin/sh\n",
        home.display(),
        other.display()
    )
}

async fn writer_after_reader_started(path: &Path) -> File {
    tokio::time::timeout(Duration::from_secs(5), async {
        loop {
            match OpenOptions::new()
                .write(true)
                .custom_flags(libc::O_NONBLOCK)
                .open(path)
            {
                Ok(writer) => return writer,
                Err(error) if error.raw_os_error() == Some(libc::ENXIO) => {
                    tokio::time::sleep(Duration::from_millis(5)).await;
                }
                Err(error) => panic!("open passwd FIFO: {error}"),
            }
        }
    })
    .await
    .expect("filesystem request did not enter passwd lookup")
}

#[tokio::test]
async fn init_does_not_retarget_a_file_request_blocked_in_user_lookup() {
    let directory = tempfile::tempdir().unwrap();
    let old = directory.path().join("old");
    let new = directory.path().join("new");
    std::fs::create_dir(&old).unwrap();
    std::fs::create_dir(&new).unwrap();
    let fifo = directory.path().join("passwd");
    let name = CString::new(fifo.as_os_str().as_encoded_bytes()).unwrap();
    assert_eq!(unsafe { libc::mkfifo(name.as_ptr(), 0o600) }, 0);
    let (port, task) = server(&fifo).await;
    init(port, json!({"defaultWorkdir": old})).await;
    let pending = tokio::spawn(rpc(port, "Stat", json!({"path": ""}), None));
    // Opening the FIFO writer proves the file worker reached lookup, which is
    // after request-context capture. Keep it empty so the worker cannot proceed.
    let mut writer = writer_after_reader_started(&fifo).await;
    init(port, json!({"defaultWorkdir": new})).await;
    writer
        .write_all(passwd(directory.path(), directory.path()).as_bytes())
        .unwrap();
    drop(writer);
    let response = pending.await.unwrap();
    assert_eq!(
        response["entry"]["path"],
        old.to_str().unwrap(),
        "{response}"
    );
    // Restore a normal database and show that a subsequent request sees new init.
    std::fs::remove_file(&fifo).unwrap();
    std::fs::write(&fifo, passwd(directory.path(), directory.path())).unwrap();
    let response = rpc(port, "Stat", json!({"path": ""}), None).await;
    assert_eq!(
        response["entry"]["path"],
        new.to_str().unwrap(),
        "{response}"
    );
    task.abort();
}

#[tokio::test]
async fn upload_keeps_captured_default_user_across_concurrent_init() {
    let directory = tempfile::tempdir().unwrap();
    let old = directory.path().join("old");
    let new = directory.path().join("new");
    std::fs::create_dir(&old).unwrap();
    std::fs::create_dir(&new).unwrap();
    let database = directory.path().join("passwd");
    let name = CString::new(database.as_os_str().as_encoded_bytes()).unwrap();
    assert_eq!(unsafe { libc::mkfifo(name.as_ptr(), 0o600) }, 0);
    let (port, task) = server(&database).await;
    let pending = tokio::spawn(async move {
        reqwest::Client::new()
            .post(format!("http://127.0.0.1:{port}/files?path=marker"))
            .header("content-type", "application/octet-stream")
            .timeout(Duration::from_secs(5))
            .body("captured request")
            .send()
            .await
            .unwrap()
    });
    // The open writer proves admission and RuntimeState capture. Replacing the
    // database pathname allows init to validate a new user while this reader
    // still owns the original FIFO. No sleep establishes the ordering.
    let mut writer = writer_after_reader_started(&database).await;
    std::fs::remove_file(&database).unwrap();
    std::fs::write(&database, passwd(&old, &new)).unwrap();
    init(port, json!({"defaultUser": "alice", "defaultWorkdir": new})).await;
    writer.write_all(passwd(&old, &new).as_bytes()).unwrap();
    drop(writer);
    let response = pending.await.unwrap();
    assert_eq!(response.status(), 200);
    let entries: Value = response.json().await.unwrap();
    assert_eq!(entries[0]["path"], old.join("marker").to_str().unwrap());
    let client = reqwest::Client::new();
    let read = client
        .get(format!("http://127.0.0.1:{port}/files"))
        .query(&[("path", old.join("marker"))])
        .send()
        .await
        .unwrap();
    assert_eq!(read.status(), 200);
    assert_eq!(read.text().await.unwrap(), "captured request");
    let fresh = rpc(port, "Stat", json!({"path": "marker"}), None).await;
    assert_eq!(fresh["code"], "not_found", "{fresh}");
    let captured = rpc(port, "Stat", json!({"path": old.join("marker")}), None).await;
    assert_eq!(captured["entry"]["owner"], "root", "{captured}");
    task.abort();
}

#[tokio::test]
async fn selected_user_home_defaults_and_symlink_dotdot_keep_guest_path_semantics() {
    let directory = tempfile::tempdir().unwrap();
    let home = directory.path().join("home");
    let other = directory.path().join("other");
    let workdir = directory.path().join("workdir");
    let outside = directory.path().join("outside");
    for path in [&home, &other, &workdir, &outside, &outside.join("nested")] {
        std::fs::create_dir(path).unwrap();
    }
    std::fs::write(home.join("marker"), b"home").unwrap();
    std::fs::write(other.join("marker"), b"alice-home").unwrap();
    std::fs::write(workdir.join("marker"), b"wrong-default").unwrap();
    std::fs::write(outside.join("marker"), b"symlink-parent").unwrap();
    symlink(outside.join("nested"), home.join("link")).unwrap();
    let database = directory.path().join("passwd");
    std::fs::write(&database, passwd(&home, &other)).unwrap();
    let (port, task) = server(&database).await;
    init(port, json!({"defaultWorkdir": workdir})).await;
    for (path, user, expected) in [
        ("marker", None, "4"),
        ("marker", Some("alice"), "10"),
        ("link/../marker", None, "4"),
    ] {
        let response = rpc(port, "Stat", json!({"path": path}), user).await;
        assert_eq!(response["entry"]["size"], expected, "{path}: {response}");
    }
    let response = rpc(
        port,
        "Stat",
        json!({"path":home.join("link/../marker")}),
        None,
    )
    .await;
    assert_eq!(response["entry"]["size"], "14", "{response}");
    for path in [".", "~", "~/."] {
        let response = rpc(port, "Stat", json!({"path": path}), Some("alice")).await;
        assert_eq!(
            response["entry"]["type"], "FILE_TYPE_DIRECTORY",
            "{response}"
        );
        let resolved = std::fs::canonicalize(response["entry"]["path"].as_str().unwrap()).unwrap();
        assert_eq!(resolved, other);
    }
    let response = rpc(port, "Stat", json!({"path": ""}), None).await;
    assert_eq!(response["entry"]["path"], workdir.to_str().unwrap());
    init(port, json!({"defaultUser": "alice"})).await;
    let response = rpc(port, "Stat", json!({"path": "marker"}), None).await;
    assert_eq!(response["entry"]["size"], "10", "{response}");
    let response = rpc(port, "Stat", json!({"path": "."}), Some("no-such-user")).await;
    assert_eq!(response["code"], "unauthenticated", "{response}");
    task.abort();
}

#[tokio::test]
async fn invalid_os_paths_keep_operation_errors_and_root_mutations_are_rejected() {
    let directory = tempfile::tempdir().unwrap();
    let database = directory.path().join("passwd");
    std::fs::write(&database, passwd(directory.path(), directory.path())).unwrap();
    let preserved = directory.path().join("preserved");
    std::fs::write(&preserved, b"untouched").unwrap();
    let (port, task) = server(&database).await;
    init(port, json!({"defaultWorkdir": directory.path()})).await;
    for method in ["Stat", "ListDir", "MakeDir", "Remove"] {
        let response = rpc(port, method, json!({"path": "nul\0path"}), None).await;
        assert_eq!(response["code"], "internal", "{method}: {response}");
    }
    // Empty mutation paths still reject a default directory resolving to root.
    let init = reqwest::Client::new()
        .post(format!("http://127.0.0.1:{port}/init"))
        .json(&json!({"defaultWorkdir": "/"}))
        .send()
        .await
        .unwrap();
    assert_eq!(init.status(), 204);
    for path in ["", "/", "/tmp/../", "/./"] {
        let response = rpc(port, "Remove", json!({"path": path}), None).await;
        assert_eq!(response["code"], "invalid_argument", "{path}: {response}");
        for body in [
            json!({"source": path, "destination": preserved}),
            json!({"source": preserved, "destination": path}),
        ] {
            let response = rpc(port, "Move", body, None).await;
            assert_eq!(response["code"], "invalid_argument", "{path}: {response}");
        }
    }
    for body in [
        json!({"source": "nul\0path", "destination": preserved}),
        json!({"source": preserved, "destination": "nul\0path"}),
    ] {
        let response = rpc(port, "Move", body, None).await;
        assert_eq!(response["code"], "internal", "{response}");
    }
    let response = rpc(port, "MakeDir", json!({"path": ""}), None).await;
    assert_eq!(response["code"], "already_exists", "{response}");
    let response = rpc(port, "MakeDir", json!({"path": "/"}), None).await;
    assert_eq!(response["code"], "already_exists", "{response}");
    assert_eq!(std::fs::read(preserved).unwrap(), b"untouched");
    task.abort();
}
