// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use std::{
    io::{Read, Stderr, Stdout, Write},
    os::fd::AsFd,
    path::PathBuf,
    time::Duration,
};

use anyhow::{anyhow, Result};
use containerd_shim_cube_rs::common::utils::Utils;
use termion::raw::{IntoRawMode, RawTerminal};
use tokio::{
    io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt},
    net::UnixStream,
    sync::mpsc::Sender,
    time::timeout,
};

use crate::{parser::LoginArgs, utils::verify_container_id};

trait IntoTerm: Write + AsFd {}
struct RawTerm {
    stdout_term: Option<RawTerminal<Stdout>>,
    stderr_term: Option<RawTerminal<Stderr>>,
}

fn check_and_get_raw_con() -> Result<RawTerm> {
    let mut raw_term = RawTerm {
        stdout_term: None,
        stderr_term: None,
    };
    if let Ok(stderr_term) = std::io::stderr().into_raw_mode() {
        raw_term.stderr_term = Some(stderr_term);
    } else if let Ok(stdout_term) = std::io::stdout().into_raw_mode() {
        raw_term.stdout_term = Some(stdout_term);
    } else {
        return Err(anyhow!("Failed to get raw terminal"));
    }
    Ok(raw_term)
}
fn restore_terminal(raw_term: RawTerm) {
    if let Some(stderr_term) = raw_term.stderr_term {
        drop(stderr_term); // This will restore the terminal to its normal state
    }
    if let Some(stdout_term) = raw_term.stdout_term {
        drop(stdout_term); // This will restore the terminal to its normal state
    }
}

fn copy_stdin_thread(tx: Sender<u8>) {
    let mut buf = [0; 1024];
    let mut stdin = std::io::stdin();
    loop {
        let r = stdin.read(&mut buf);
        match r {
            Ok(0) => break, // EOF
            Ok(n) => {
                for x in &buf[..n] {
                    if let Err(e) = tx.blocking_send(*x) {
                        println!("Error sending to channel: {}", e);
                        break;
                    }
                }
            }
            Err(e) => {
                println!("Error reading stdin: {}", e);
                break;
            }
        }
    }
}

pub async fn execute(args: LoginArgs) -> Result<()> {
    let id = verify_container_id(&args.sandbox_id)
        .map_err(|e| anyhow!("cubebox id is invalid: {}", e))?;
    let vsock_path = Utils::vsock_path(&id);
    let conn = hybrid_vsock_dialer(
        &vsock_path,
        args.port,
        Duration::from_secs(args.timeout.into()),
    )
    .await?;
    let (mut reader, mut writer) = conn.into_split();

    let mut stdout = tokio::io::stdout();
    let (oneshot_tx, mut oneshot_rx) = tokio::sync::mpsc::channel(1);

    let (stdin_tx, mut stdin_rx) = tokio::sync::mpsc::channel::<u8>(1024);

    // Set the console to raw modez
    let con = check_and_get_raw_con()?;
    // use thread to copy stdin to writer
    std::thread::spawn(move || {
        copy_stdin_thread(stdin_tx);
    });
    let copy_stdin: tokio::task::JoinHandle<()> = tokio::spawn(async move {
        loop {
            tokio::select! {
                r = stdin_rx.recv() => {
                    if let Some(c) = r {
                        if let Err(e) = writer.write_all(&[c]).await{
                            println!("copy stdin Error: {}", e);
                            break;
                        }
                    } else {
                        break;
                    }

                },
                _ = oneshot_rx.recv() => {
                    break;
                }

            }
        }
        oneshot_rx.close();
    });

    let copy_stdout = tokio::spawn(async move {
        let mut rbuf = [0; 1024];
        loop {
            let r = reader.read(&mut rbuf).await;
            match r {
                Ok(0) => break,
                Ok(n) => {
                    let x = stdout.write_all(&rbuf[..n]).await;
                    if x.is_err() {
                        break;
                    }
                    let x = stdout.flush().await;
                    if x.is_err() {
                        break;
                    }
                }
                Err(e) => {
                    println!("copy stdout Error: {}", e);
                    break;
                }
            }
        }
        oneshot_tx.send(1).await.ok();
    });

    copy_stdout.await?;
    copy_stdin.await?;

    restore_terminal(con);
    Ok(())
}

async fn hybrid_vsock_dialer(
    vsock_path: &PathBuf,
    port: u32,
    timeout_duration: Duration,
) -> Result<UnixStream> {
    if port == 0 {
        return Err(anyhow!("Port not specified"));
    }

    let mut conn = timeout(timeout_duration, UnixStream::connect(vsock_path)).await??;

    conn.write_all(format!("CONNECT {}\n", port).as_bytes())
        .await?;

    let mut reader = tokio::io::BufReader::new(conn);
    let mut response = String::new();
    reader.read_line(&mut response).await?;

    if response.contains("OK") {
        Ok(reader.into_inner())
    } else {
        Err(anyhow!(
            "HybridVsock trivial handshake failed. port: {}, response: {}",
            port,
            response
        ))
    }
}
