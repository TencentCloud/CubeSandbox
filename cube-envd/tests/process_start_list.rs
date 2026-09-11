// SPDX-License-Identifier: Apache-2.0

use reqwest::Client;
use serde_json::{json, Value};

use cube_envd::server::ServerPhase;

mod support;

fn envelope(value: &Value) -> Vec<u8> {
    let payload = serde_json::to_vec(value).unwrap();
    let mut body = vec![0];
    body.extend_from_slice(&(payload.len() as u32).to_be_bytes());
    body.extend(payload);
    body
}

async fn first_frame(response: &mut reqwest::Response) -> Vec<u8> {
    let mut bytes = Vec::new();
    loop {
        if bytes.len() >= 5 {
            let length = u32::from_be_bytes(bytes[1..5].try_into().unwrap()) as usize;
            if bytes.len() >= length + 5 {
                bytes.truncate(length + 5);
                return bytes;
            }
        }
        bytes.extend(response.chunk().await.unwrap().expect("first frame"));
    }
}

async fn start(client: &Client, port: u16, request: Value) -> Vec<(u8, Value)> {
    let mut response = client
        .post(format!("http://127.0.0.1:{port}/process.Process/Start"))
        .header("content-type", "application/connect+json")
        .header("connect-protocol-version", "1")
        .body(envelope(&request))
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 200);
    let bytes = first_frame(&mut response).await;
    let mut body = bytes.as_slice();
    let mut frames = Vec::new();
    while !body.is_empty() {
        assert!(body.len() >= 5);
        let len = u32::from_be_bytes(body[1..5].try_into().unwrap()) as usize;
        frames.push((body[0], serde_json::from_slice(&body[5..5 + len]).unwrap()));
        body = &body[5 + len..];
    }
    frames
}

async fn list(client: &Client, port: u16) -> Vec<Value> {
    let response = client
        .post(format!("http://127.0.0.1:{port}/process.Process/List"))
        .header("content-type", "application/json")
        .header("connect-protocol-version", "1")
        .body("{}")
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 200);
    let body: Value = response.json().await.unwrap();
    body["processes"].as_array().cloned().unwrap_or_default()
}

#[tokio::test]
async fn list_starts_empty_and_never_adopts_external_pids() {
    let (port, _, server) = support::spawn_envd_with_state(ServerPhase::Ready).await;
    let client = Client::builder().no_proxy().build().unwrap();
    assert!(list(&client, port).await.is_empty());
    server.abort();
}

#[tokio::test]
async fn empty_arguments_are_limited_by_exec_not_a_daemon_allocation_budget() {
    let (port, _, server) = support::spawn_envd_with_state(ServerPhase::Ready).await;
    let client = Client::builder().no_proxy().build().unwrap();
    let frames = start(
        &client,
        port,
        json!({"process":{"cmd":"/bin/true", "args":vec![""; 40_000]}}),
    )
    .await;
    assert!(frames[0].1["event"].get("start").is_some());
    tokio::time::timeout(std::time::Duration::from_secs(5), async {
        while !list(&client, port).await.is_empty() {
            tokio::task::yield_now().await;
        }
    })
    .await
    .unwrap();
    server.abort();
}

#[tokio::test]
async fn direct_argv_start_first_and_live_list_preserve_original_config() {
    let (port, state, server) = support::spawn_envd_with_state(ServerPhase::Ready).await;
    let client = Client::builder().no_proxy().build().unwrap();
    assert!(
        list(&client, port).await.is_empty(),
        "external PIDs are unmanaged"
    );
    let config = json!({"cmd":"/bin/sleep", "args":["0.3"], "envs":{"LITERAL":"$(false); *"}});
    let frames = start(&client, port, json!({"process":config,"tag":"original"})).await;
    assert_eq!(
        frames[0].0, 0,
        "StartEvent must precede the trailer: {frames:?}"
    );
    let pid = frames[0].1["event"]["start"]["pid"].as_u64().unwrap();
    assert!(pid > 0);
    assert_eq!(frames.len(), 1, "read only Start while the process is live");
    let processes = list(&client, port).await;
    assert_eq!(processes.len(), 1);
    assert_eq!(
        processes[0],
        json!({"config":config,"pid":pid,"tag":"original"})
    );
    assert_eq!(
        std::fs::read(format!("/proc/{pid}/cmdline")).unwrap(),
        b"/bin/sleep\x000.3\x00"
    );
    tokio::time::timeout(std::time::Duration::from_secs(3), async {
        while !list(&client, port).await.is_empty() {
            tokio::time::sleep(std::time::Duration::from_millis(10)).await;
        }
    })
    .await
    .unwrap();
    assert!(
        !std::path::Path::new(&format!("/proc/{pid}")).exists(),
        "leader must be reaped"
    );
    assert!(
        state.lifecycle.is_ready(),
        "ordinary user exit is not EnvdFailure"
    );
    server.abort();
}

#[tokio::test]
async fn duplicate_present_tags_and_absent_tags_can_coexist() {
    let (port, _, server) = support::spawn_envd_with_state(ServerPhase::Ready).await;
    let client = Client::builder().no_proxy().build().unwrap();
    for tag in ["", "shared"] {
        let request = json!({"process":{"cmd":"/bin/sleep","args":["1"]},"tag":tag});
        let results =
            futures::future::join_all((0..8).map(|_| start(&client, port, request.clone()))).await;
        let winners = results.iter().filter(|frames| frames[0].0 == 0).count();
        assert_eq!(winners, 8, "tag={tag:?}: {results:?}");
    }
    for _ in 0..2 {
        assert_eq!(
            start(
                &client,
                port,
                json!({"process":{"cmd":"/bin/sleep","args":["1"]}})
            )
            .await[0]
                .0,
            0
        );
    }
    let processes = list(&client, port).await;
    assert_eq!(processes.len(), 18);
    assert_eq!(
        processes.iter().filter(|p| p.get("tag").is_none()).count(),
        2
    );
    assert_eq!(
        processes
            .iter()
            .filter(|p| p.get("tag") == Some(&json!("")))
            .count(),
        8
    );
    assert!(processes
        .windows(2)
        .all(|p| p[0]["pid"].as_u64() < p[1]["pid"].as_u64()));
    tokio::time::sleep(std::time::Duration::from_millis(1100)).await;
    assert!(list(&client, port).await.is_empty());
    assert_eq!(
        start(
            &client,
            port,
            json!({"process":{"cmd":"/bin/true"},"tag":"shared"})
        )
        .await[0]
            .0,
        0
    );
    server.abort();
}

#[tokio::test]
async fn invalid_start_has_no_pid_and_releases_tag() {
    let (port, _, server) = support::spawn_envd_with_state(ServerPhase::Ready).await;
    let client = Client::builder().no_proxy().build().unwrap();
    for config in [
        json!({"cmd":"bad\u{0}command"}),
        json!({"cmd":"/bin/true","args":["bad\u{0}arg"]}),
        json!({"cmd":"/bin/true","cwd":"bad\u{0}cwd"}),
        json!({"cmd":"/bin/true","cwd":"/cube-envd-missing-directory"}),
        json!({"cmd":"/bin/true","envs":{"KEY":"bad\u{0}value"}}),
    ] {
        let frames = start(&client, port, json!({"process":config,"tag":"reusable"})).await;
        assert_eq!(frames.len(), 1, "config={config}, frames={frames:?}");
        assert_eq!(frames[0].0, 2);
        assert_eq!(
            frames[0].1["error"]["code"], "invalid_argument",
            "{frames:?}"
        );
        assert!(list(&client, port).await.is_empty());
    }
    let frames = start(
        &client,
        port,
        json!({"process":{"cmd":"/bin/sleep","args":["0.1"]},"tag":"reusable"}),
    )
    .await;
    assert_eq!(frames[0].0, 0, "{frames:?}");
    server.abort();
}

#[tokio::test]
async fn absolute_exec_preserves_child_environment_and_setup_precedes_user_code() {
    use std::os::unix::fs::PermissionsExt;
    let directory = tempfile::tempdir().unwrap();
    let impostor = directory.path().join("python3");
    std::fs::write(&impostor, b"#!/bin/sh\nexit 99\n").unwrap();
    std::fs::set_permissions(&impostor, std::fs::Permissions::from_mode(0o755)).unwrap();
    let output = directory.path().join("observed.json");
    let script = "import os,json,sys,time; json.dump(dict(argv=sys.argv[2:],cwd=os.getcwd(),path=os.environ['PATH'],init=os.environ['FROM_INIT'],user=os.getuid(),gid=os.getgid(),nice=os.getpriority(os.PRIO_PROCESS,0),oom=open('/proc/self/oom_score_adj').read().strip(),cgroup=open('/proc/self/cgroup').read().strip(),weight=open('/sys/fs/cgroup/user/cpu.weight').read().strip()),open(sys.argv[1],'w')); time.sleep(1)";
    let (port, _, server) = support::spawn_envd_with_state(ServerPhase::Ready).await;
    let client = Client::builder().no_proxy().build().unwrap();
    let child_path = directory.path().to_str().unwrap();
    assert_eq!(client.post(format!("http://127.0.0.1:{port}/init"))
        .json(&json!({"defaultWorkdir":child_path,"envVars":{"PATH":"/no-init-executables","FROM_INIT":"first"}}))
        .send().await.unwrap().status(), 204);
    let config = json!({"cmd":"/usr/bin/python3","args":["-c",script,output,"literal spaces","$(false)","*",";"],"envs":{"PATH":child_path}});
    let frames = start(&client, port, json!({"process":config})).await;
    assert_eq!(frames[0].0, 0, "{frames:?}");
    tokio::time::timeout(std::time::Duration::from_secs(3), async {
        loop {
            if let Ok(bytes) = std::fs::read(&output) {
                if let Ok(observed) = serde_json::from_slice::<Value>(&bytes) {
                    break observed;
                }
            }
            tokio::time::sleep(std::time::Duration::from_millis(10)).await;
        }
    }).await.map(|observed| {
        assert_eq!(observed, json!({"argv":["literal spaces","$(false)","*",";"],"cwd":child_path,"path":child_path,"init":"first","user":0,"gid":0,"nice":0,"oom":"100","cgroup":"0::/user","weight":"50"}));
    }).unwrap();
    assert_eq!(list(&client, port).await[0]["config"], config);
    server.abort();
}

#[tokio::test]
async fn controls_cannot_target_an_unmanaged_pid() {
    let (port, _, server) = support::spawn_envd_with_state(ServerPhase::Ready).await;
    let client = Client::builder().no_proxy().build().unwrap();
    for method in ["SendInput", "CloseStdin", "Update", "SendSignal"] {
        let response = client
            .post(format!("http://127.0.0.1:{port}/process.Process/{method}"))
            .header("content-type", "application/json")
            .header("connect-protocol-version", "1")
            .json(&json!({"process":{"pid":std::process::id()},"signal":9}))
            .send()
            .await
            .unwrap();
        assert_eq!(response.status(), 404);
        assert_eq!(response.json::<Value>().await.unwrap()["code"], "not_found");
    }
    let response = client
        .post(format!("http://127.0.0.1:{port}/process.Process/Connect"))
        .header("content-type", "application/connect+json")
        .header("connect-protocol-version", "1")
        .body(envelope(&json!({"process":{"pid":std::process::id()}})))
        .send()
        .await
        .unwrap()
        .bytes()
        .await
        .unwrap();
    assert_eq!(response[0], 2);
    let trailer: Value = serde_json::from_slice(&response[5..]).unwrap();
    assert_eq!(trailer["error"]["code"], "not_found");
    assert!(list(&client, port).await.is_empty());
    server.abort();
}

#[tokio::test]
async fn protobuf_start_and_list_round_trip_original_config() {
    use buffa::Message;
    use cube_envd::proto::process::{ListResponse, StartRequest, StartResponse};
    let (port, _, server) = support::spawn_envd_with_state(ServerPhase::Ready).await;
    let client = Client::builder().no_proxy().build().unwrap();
    let request: StartRequest =
        serde_json::from_value(json!({"process":{"cmd":"/bin/sleep","args":["0.5"]},"tag":""}))
            .unwrap();
    let payload = request.encode_to_vec();
    let mut body = vec![0];
    body.extend_from_slice(&(payload.len() as u32).to_be_bytes());
    body.extend(payload);
    let mut response = client
        .post(format!("http://127.0.0.1:{port}/process.Process/Start"))
        .header("content-type", "application/connect+proto")
        .header("connect-protocol-version", "1")
        .body(body)
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 200);
    let bytes = first_frame(&mut response).await;
    assert_eq!(bytes[0], 0);
    let length = u32::from_be_bytes(bytes[1..5].try_into().unwrap()) as usize;
    let event = StartResponse::decode(&mut &bytes[5..5 + length]).unwrap();
    let event = serde_json::to_value(event).unwrap();
    let pid = event["event"]["start"]["pid"].as_u64().unwrap();

    let response = client
        .post(format!("http://127.0.0.1:{port}/process.Process/List"))
        .header("content-type", "application/proto")
        .header("connect-protocol-version", "1")
        .body(Vec::new())
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 200);
    let bytes = response.bytes().await.unwrap();
    let listed = ListResponse::decode(&mut bytes.as_ref()).unwrap();
    assert_eq!(listed.processes.len(), 1);
    assert_eq!(listed.processes[0].pid as u64, pid);
    assert_eq!(listed.processes[0].config, request.process);
    assert_eq!(listed.processes[0].tag, Some(String::new()));
    server.abort();
}

#[tokio::test]
async fn discarding_start_response_keeps_wait_owner_and_terminal_cleanup() {
    let (port, state, server) = support::spawn_envd_with_state(ServerPhase::Ready).await;
    let client = Client::builder().no_proxy().build().unwrap();
    let response = client
        .post(format!("http://127.0.0.1:{port}/process.Process/Start"))
        .header("content-type", "application/connect+json")
        .header("connect-protocol-version", "1")
        .body(envelope(
            &json!({"process":{"cmd":"/bin/sleep","args":["0.5"]},"tag":"disconnected"}),
        ))
        .send()
        .await
        .unwrap();
    drop(response);
    let processes = list(&client, port).await;
    assert_eq!(processes.len(), 1);
    let pid = processes[0]["pid"].as_u64().unwrap();
    tokio::time::timeout(std::time::Duration::from_secs(3), async {
        while !list(&client, port).await.is_empty() {
            tokio::time::sleep(std::time::Duration::from_millis(10)).await;
        }
    })
    .await
    .unwrap();
    assert!(!std::path::Path::new(&format!("/proc/{pid}")).exists());
    assert!(state.lifecycle.is_ready());
    assert_eq!(
        start(
            &client,
            port,
            json!({"process":{"cmd":"/bin/true"},"tag":"disconnected"})
        )
        .await[0]
            .0,
        0
    );
    server.abort();
}

#[tokio::test]
async fn target_credentials_are_applied_before_user_code() {
    use std::os::unix::fs::PermissionsExt;
    let directory = tempfile::tempdir().unwrap();
    std::fs::set_permissions(directory.path(), std::fs::Permissions::from_mode(0o777)).unwrap();
    let marker = directory.path().join("identity");
    let (port, _, server) = support::spawn_envd_with_state(ServerPhase::Ready).await;
    let client = Client::builder().no_proxy().build().unwrap();
    let config = json!({"cmd":"python3","cwd":"/","args":["-c",
        "import os,json,sys; json.dump([os.getuid(),os.getgid(),os.getgroups()],open(sys.argv[1],'w'))",marker]});
    let response = client
        .post(format!("http://127.0.0.1:{port}/process.Process/Start"))
        .basic_auth("nobody", Some(""))
        .header("content-type", "application/connect+json")
        .header("connect-protocol-version", "1")
        .body(envelope(&json!({"process":config})))
        .send()
        .await
        .unwrap();
    let bytes = response.bytes().await.unwrap();
    assert_eq!(bytes[0], 0, "{:?}", String::from_utf8_lossy(&bytes));
    let observed = tokio::time::timeout(std::time::Duration::from_secs(2), async {
        loop {
            if let Ok(bytes) = std::fs::read(&marker) {
                if let Ok(value) = serde_json::from_slice::<Value>(&bytes) {
                    break value;
                }
            }
            tokio::time::sleep(std::time::Duration::from_millis(5)).await;
        }
    })
    .await
    .unwrap();
    assert_eq!(observed, json!([65534, 65534, [65534]]));
    server.abort();
}
