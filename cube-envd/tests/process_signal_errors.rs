// SPDX-License-Identifier: Apache-2.0

use base64::{engine::general_purpose::STANDARD, Engine};
use cube_envd::server::ServerPhase;
use reqwest::Client;
use serde_json::json;
use std::time::Duration;
#[path = "support/process.rs"]
mod process;
mod support;
use process::{unary as rpc, Stream};

impl Stream {
    async fn natural_end(&mut self) {
        let mut stdout = vec![];
        loop {
            let (flags, event) = self.next_with_timeout(3).await;
            assert_eq!(flags, 0, "{event}");
            if let Some(end) = event["event"].get("end") {
                assert_eq!(end["exited"], true);
                assert_eq!(end["status"], "exit status 0");
                break;
            }
            stdout.extend(
                STANDARD
                    .decode(event["event"]["data"]["stdout"].as_str().unwrap())
                    .unwrap(),
            );
        }
        assert_eq!(stdout, b"natural\n");
        assert_eq!(self.next_with_timeout(3).await, (2, json!({})));
    }
}

// The filter only changes kill(leader, ...) on this dedicated test thread.
// The leader has already been spawned. No TSYNC, global setting, other PID,
// or production fault-injection hook is involved; the filter dies with thread.
fn reject_leader_kill(pid: u32, errno: i32) {
    let mut filter = [
        libc::sock_filter {
            code: (libc::BPF_LD | libc::BPF_W | libc::BPF_ABS) as u16,
            jt: 0,
            jf: 0,
            k: 0,
        },
        libc::sock_filter {
            code: (libc::BPF_JMP | libc::BPF_JEQ | libc::BPF_K) as u16,
            jt: 0,
            jf: 3,
            k: libc::SYS_kill as u32,
        },
        // Linux seccomp_data.args[0] begins at byte 16.
        libc::sock_filter {
            code: (libc::BPF_LD | libc::BPF_W | libc::BPF_ABS) as u16,
            jt: 0,
            jf: 0,
            k: 16,
        },
        libc::sock_filter {
            code: (libc::BPF_JMP | libc::BPF_JEQ | libc::BPF_K) as u16,
            jt: 0,
            jf: 1,
            k: pid,
        },
        libc::sock_filter {
            code: (libc::BPF_RET | libc::BPF_K) as u16,
            jt: 0,
            jf: 0,
            k: libc::SECCOMP_RET_ERRNO | errno as u32,
        },
        libc::sock_filter {
            code: (libc::BPF_RET | libc::BPF_K) as u16,
            jt: 0,
            jf: 0,
            k: libc::SECCOMP_RET_ALLOW,
        },
    ];
    let program = libc::sock_fprog {
        len: filter.len() as u16,
        filter: filter.as_mut_ptr(),
    };
    // SAFETY: prctl consumes the valid filter array synchronously; the filter
    // is copied by the kernel and does not reference Rust memory afterward.
    unsafe {
        assert_eq!(
            libc::prctl(libc::PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0),
            0,
            "{}",
            std::io::Error::last_os_error()
        );
        assert_eq!(
            libc::prctl(libc::PR_SET_SECCOMP, libc::SECCOMP_MODE_FILTER, &program),
            0,
            "{}",
            std::io::Error::last_os_error()
        );
    }
}

#[test]
fn syscall_signal_errors_keep_registry_and_terminal_wait_authoritative() {
    for (errno, status, code) in [
        (libc::EPERM, 500, "internal"),
        (libc::ESRCH, 404, "not_found"),
    ] {
        // A current-thread runtime ensures the public HTTP handler reaches
        // the real kill syscall through this thread's narrowly scoped filter.
        std::thread::spawn(move || {
            tokio::runtime::Builder::new_current_thread().enable_all().build().unwrap().block_on(async {
                let (port,_,server) = support::spawn_envd_with_state(ServerPhase::Ready).await;
                let client = Client::builder().no_proxy().build().unwrap();
                let directory = tempfile::tempdir().unwrap();
                let release = directory.path().join("release");
                let request = json!({"tag":"signal-error","process":{"cmd":"/usr/bin/python3","args":["-u","-c",
                    format!("import os,time\nwhile not os.path.exists({release:?}): time.sleep(.01)\nprint('natural')")]}});
                let mut owner = Stream::open(&client,port,"Start",request).await;
                let first = owner.next_with_timeout(3).await;
                let pid = first.1["event"]["start"]["pid"].as_u64().unwrap() as u32;
                reject_leader_kill(pid,errno);
                for signal in [9,15] {
                    let (actual,error) = rpc(&client,port,"SendSignal",json!({"process":{"pid":pid},"signal":signal})).await;
                    assert_eq!(actual,status,"{error}");
                    assert_eq!(error["code"],code);
                }
                let listed = rpc(&client,port,"List",json!({})).await.1;
                assert_eq!(listed["processes"].as_array().unwrap().len(),1);
                assert_eq!(listed["processes"][0]["pid"],pid);
                assert_eq!(listed["processes"][0]["tag"], "signal-error", "failed signal must retain the original tag");
                let mut follower = Stream::open(&client,port,"Connect",json!({"process":{"tag":"signal-error"}})).await;
                assert_eq!(follower.next_with_timeout(3).await,first);
                assert!(tokio::time::timeout(Duration::from_millis(100),owner.next_with_timeout(3)).await.is_err(),"signal failure is not terminal");
                std::fs::write(release,b"").unwrap();
                owner.natural_end().await;
                follower.natural_end().await;
                assert_eq!(rpc(&client,port,"List",json!({})).await,(200,json!({})));
                assert_eq!(rpc(&client,port,"SendSignal",json!({"process":{"tag":"signal-error"},"signal":9})).await.1["code"],"not_found");
                let mut reused = Stream::open(&client,port,"Start",json!({"tag":"signal-error","process":{"cmd":"/bin/echo","args":["natural"]}})).await;
                assert!(reused.next_with_timeout(3).await.1["event"]["start"]["pid"].is_number());
                reused.natural_end().await;
                assert_eq!(client.get(format!("http://127.0.0.1:{port}/health")).send().await.unwrap().status(),204);
                server.abort();
            });
        }).join().unwrap();
    }
}
