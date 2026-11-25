// Copyright 2026 Tencent Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//! End-to-end integration tests against a real `nbdkit` server binary.
//! The binary is looked up in `$NBDKIT`, then `~/workloads/nbdkit/sbin/nbdkit`
//! (installed by `scripts/prepare_nbdkit.sh`), then `PATH`. Tests are skipped
//! when none is found.

use std::fs::{self, OpenOptions};
use std::os::unix::io::AsRawFd;
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::thread;
use std::time::{Duration, Instant};

use nbd::{NbdClient, NbdClientOptions, NbdConn, NbdEngine, NbdSocketOptions};
use vmm_sys_util::tempdir::TempDir;

fn find_nbdkit() -> Option<PathBuf> {
    if let Some(path) = std::env::var_os("NBDKIT") {
        return Some(PathBuf::from(path));
    }
    if let Some(home) = std::env::var_os("HOME") {
        let workload = Path::new(&home).join("workloads/nbdkit/sbin/nbdkit");
        if workload.is_file() {
            return Some(workload);
        }
    }
    Command::new("nbdkit")
        .arg("--version")
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status()
        .ok()
        .filter(|s| s.success())
        .map(|_| PathBuf::from("nbdkit"))
}

struct NbdkitGuard {
    child: Child,
    sock: PathBuf,
}

impl NbdkitGuard {
    fn spawn(nbdkit: &Path, sock: &Path, extra_args: &[&str]) -> Self {
        let _ = fs::remove_file(sock);
        let mut cmd = Command::new(nbdkit);
        cmd.arg("-f")
            .arg("-U")
            .arg(sock)
            .args(extra_args)
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::null());
        let mut child = cmd.spawn().expect("failed to spawn nbdkit");

        let deadline = Instant::now() + Duration::from_secs(5);
        while Instant::now() < deadline {
            if let Ok(Some(status)) = child.try_wait() {
                panic!("nbdkit exited early with {status}");
            }
            if sock.exists() && std::os::unix::net::UnixStream::connect(sock).is_ok() {
                return Self {
                    child,
                    sock: sock.to_path_buf(),
                };
            }
            thread::sleep(Duration::from_millis(15));
        }
        let _ = child.kill();
        panic!("timed out waiting for nbdkit socket {}", sock.display());
    }

    fn kill_and_cleanup(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
        let _ = fs::remove_file(&self.sock);
    }
}

impl Drop for NbdkitGuard {
    fn drop(&mut self) {
        self.kill_and_cleanup();
    }
}

fn wait_completions(engine: &mut NbdEngine, expected: usize) -> Vec<(u64, i32)> {
    let mut got = Vec::new();
    let mut pollfd = libc::pollfd {
        fd: engine.notifier().as_raw_fd(),
        events: libc::POLLIN,
        revents: 0,
    };
    while got.len() < expected {
        got.extend(engine.complete());
        if got.len() >= expected {
            break;
        }
        let r = unsafe { libc::poll(&mut pollfd, 1, 5000) };
        assert!(r > 0, "timed out waiting for nbdkit completions");
        let _ = engine.notifier().read();
    }
    got
}

#[test]
fn real_nbdkit_file_multiqueue_and_blocksize_filter() {
    let Some(nbdkit) = find_nbdkit() else {
        eprintln!("skipping real_nbdkit_file_multiqueue_and_blocksize_filter: nbdkit not found");
        return;
    };

    let dir = TempDir::new().unwrap();
    let backing_path = dir.as_path().join("disk.raw");
    let f = OpenOptions::new()
        .create(true)
        .truncate(true)
        .read(true)
        .write(true)
        .open(&backing_path)
        .unwrap();
    f.set_len(4 * 1024 * 1024).unwrap();
    drop(f);

    // 1. Test blocksize-policy filter (advertises min=4096, max=65536 to client)
    //    AND blocksize filter (advertises min=1, max=0xffffffff to client).
    let sock = dir.as_path().join("file.sock");
    let file_arg = format!("file={}", backing_path.display());
    let _srv = NbdkitGuard::spawn(
        &nbdkit,
        &sock,
        &[
            "--filter=blocksize-policy",
            "file",
            &file_arg,
            "blocksize-minimum=4096",
            "blocksize-preferred=4096",
            "blocksize-maximum=65536",
        ],
    );

    for prefer_uring in [false, true] {
        let uri = format!("nbd+unix:///vol0?socket={}", sock.display());
        let client = NbdClient::open(
            &uri,
            NbdClientOptions {
                readonly: false,
                num_queues: 2,
                prefer_uring,
                socket_override: None,
            },
        )
        .unwrap();

        assert_eq!(client.export_info().size, 4 * 1024 * 1024);
        assert_eq!(client.export_info().min_block, 4096);
        assert_eq!(client.export_info().max_block, 65536);

        let mut q0 = client.new_engine().unwrap();
        let mut q1 = client.new_engine().unwrap();

        // 256 KiB write on queue 0 (splits into four 64 KiB NBD chunks) and 128 KiB on queue 1.
        let pattern0: Vec<u8> = (0..256 * 1024)
            .map(|i| ((i * 3 + 1) & 0xff) as u8)
            .collect();
        let pattern1: Vec<u8> = (0..128 * 1024)
            .map(|i| ((i * 11 + 7) & 0xff) as u8)
            .collect();

        unsafe {
            q0.write_vectored(
                0,
                vec![libc::iovec {
                    iov_base: pattern0.as_ptr() as *mut libc::c_void,
                    iov_len: pattern0.len(),
                }],
                1,
            )
            .unwrap();
            q1.write_vectored(
                512 * 1024,
                vec![libc::iovec {
                    iov_base: pattern1.as_ptr() as *mut libc::c_void,
                    iov_len: pattern1.len(),
                }],
                2,
            )
            .unwrap();
        }
        assert_eq!(wait_completions(&mut q0, 1), vec![(1, 256 * 1024)]);
        assert_eq!(wait_completions(&mut q1, 1), vec![(2, 128 * 1024)]);

        // Write zeroes over [64K, 128K) and flush.
        q0.write_zeroes(64 * 1024, 64 * 1024, 3).unwrap();
        assert_eq!(wait_completions(&mut q0, 1), vec![(3, 0)]);
        q0.fsync(Some(4)).unwrap();
        assert_eq!(wait_completions(&mut q0, 1), vec![(4, 0)]);

        // Read back across queue 1 and verify against both pattern and raw host file.
        let mut read_buf = vec![0xffu8; 256 * 1024];
        unsafe {
            q1.read_vectored(
                0,
                vec![libc::iovec {
                    iov_base: read_buf.as_mut_ptr().cast(),
                    iov_len: read_buf.len(),
                }],
                5,
            )
            .unwrap();
        }
        assert_eq!(wait_completions(&mut q1, 1), vec![(5, 256 * 1024)]);
        assert_eq!(&read_buf[..64 * 1024], &pattern0[..64 * 1024]);
        assert!(read_buf[64 * 1024..128 * 1024].iter().all(|&b| b == 0));
        assert_eq!(&read_buf[128 * 1024..], &pattern0[128 * 1024..]);

        let host_bytes = fs::read(&backing_path).unwrap();
        assert_eq!(&host_bytes[..256 * 1024], &read_buf[..]);
        assert_eq!(&host_bytes[512 * 1024..640 * 1024], &pattern1[..]);
    }

    // 2. Also verify nbdkit's `--filter=blocksize` which advertises min=1, max=0xffffffff (-1).
    let sock_bs = dir.as_path().join("blocksize.sock");
    let _srv_bs = NbdkitGuard::spawn(
        &nbdkit,
        &sock_bs,
        &["--filter=blocksize", "file", &file_arg, "minblock=4096"],
    );
    let uri_bs = format!("nbd+unix:///vol0?socket={}", sock_bs.display());
    let client_bs = NbdClient::open(&uri_bs, NbdClientOptions::default()).unwrap();
    assert_eq!(client_bs.export_info().min_block, 1);
    assert_eq!(client_bs.export_info().max_block, u32::MAX);
    let mut eng_bs = client_bs.new_engine().unwrap();
    let mut probe_buf = vec![0u8; 4096];
    unsafe {
        eng_bs
            .read_vectored(
                0,
                vec![libc::iovec {
                    iov_base: probe_buf.as_mut_ptr().cast(),
                    iov_len: probe_buf.len(),
                }],
                99,
            )
            .unwrap();
    }
    assert_eq!(wait_completions(&mut eng_bs, 1), vec![(99, 4096)]);
}

#[test]
fn real_nbdkit_rust_plugin_and_restart_reconnect() {
    let Some(nbdkit) = find_nbdkit() else {
        eprintln!("skipping real_nbdkit_rust_plugin_and_restart_reconnect: nbdkit not found");
        return;
    };

    // 1. If the compiled Rust plugin example exists in /data/nbdkit, verify direct Rust-plugin interop.
    let rust_plugin = Path::new("/data/nbdkit/plugins/rust/target/release/examples/libramdisk.so");
    if rust_plugin.is_file() {
        let dir = TempDir::new().unwrap();
        let sock = dir.as_path().join("rust-plugin.sock");
        let _srv = NbdkitGuard::spawn(&nbdkit, &sock, &[rust_plugin.to_str().unwrap()]);

        let uri = format!("nbd+unix://?socket={}", sock.display());
        let mut conn =
            NbdConn::connect(&uri.parse().unwrap(), &NbdSocketOptions::default()).unwrap();
        assert_eq!(conn.info().size, 100 * 1024 * 1024);
        conn.write_at(8192, b"hello-nbdkit-rust-plugin").unwrap();
        let mut out = [0u8; 24];
        conn.read_at(8192, &mut out).unwrap();
        assert_eq!(&out, b"hello-nbdkit-rust-plugin");
        conn.disconnect();
    }

    // 2. Kill and restart nbdkit on the same Unix socket while an NbdEngine stays open!
    let dir = TempDir::new().unwrap();
    let backing_path = dir.as_path().join("persist.raw");
    let f = OpenOptions::new()
        .create(true)
        .truncate(true)
        .read(true)
        .write(true)
        .open(&backing_path)
        .unwrap();
    f.set_len(2 * 1024 * 1024).unwrap();
    drop(f);

    let sock = dir.as_path().join("restart.sock");
    let file_arg = format!("file={}", backing_path.display());

    for prefer_uring in [false, true] {
        let mut srv1 = NbdkitGuard::spawn(&nbdkit, &sock, &["file", &file_arg]);
        let uri = format!(
            "nbd+unix:///persist?socket={}&reconnect_timeout=5",
            sock.display()
        );
        let client = NbdClient::open(
            &uri,
            NbdClientOptions {
                readonly: false,
                num_queues: 1,
                prefer_uring,
                socket_override: None,
            },
        )
        .unwrap();
        let mut engine = client.new_engine().unwrap();

        let msg1 = vec![0xA5u8; 16 * 1024];
        unsafe {
            engine
                .write_vectored(
                    0,
                    vec![libc::iovec {
                        iov_base: msg1.as_ptr() as *mut libc::c_void,
                        iov_len: msg1.len(),
                    }],
                    10,
                )
                .unwrap();
        }
        assert_eq!(wait_completions(&mut engine, 1), vec![(10, 16 * 1024)]);

        // Kill nbdkit abruptly and respawn it after 80ms in a background thread so the
        // client hits a dead socket, backs off, reconnects to the new nbdkit, and replays!
        // Keep the spawning thread alive until verification finishes (Linux PDEATHSIG is per-TID).
        srv1.kill_and_cleanup();
        let (stop_tx, stop_rx) = std::sync::mpsc::channel::<()>();
        let nbdkit_clone = nbdkit.clone();
        let sock_clone = sock.clone();
        let file_arg_clone = file_arg.clone();
        let respawn = thread::spawn(move || {
            thread::sleep(Duration::from_millis(80));
            let _srv2 = NbdkitGuard::spawn(&nbdkit_clone, &sock_clone, &["file", &file_arg_clone]);
            let _ = stop_rx.recv();
        });

        let msg2 = vec![0x5Au8; 16 * 1024];
        unsafe {
            engine
                .write_vectored(
                    16 * 1024,
                    vec![libc::iovec {
                        iov_base: msg2.as_ptr() as *mut libc::c_void,
                        iov_len: msg2.len(),
                    }],
                    11,
                )
                .unwrap();
        }
        assert_eq!(wait_completions(&mut engine, 1), vec![(11, 16 * 1024)]);

        let mut verify = vec![0u8; 32 * 1024];
        unsafe {
            engine
                .read_vectored(
                    0,
                    vec![libc::iovec {
                        iov_base: verify.as_mut_ptr().cast(),
                        iov_len: verify.len(),
                    }],
                    12,
                )
                .unwrap();
        }
        assert_eq!(wait_completions(&mut engine, 1), vec![(12, 32 * 1024)]);
        assert_eq!(&verify[..16 * 1024], &msg1[..]);
        assert_eq!(&verify[16 * 1024..], &msg2[..]);

        drop(engine);
        drop(client);
        let _ = stop_tx.send(());
        respawn.join().unwrap();
    }
}
