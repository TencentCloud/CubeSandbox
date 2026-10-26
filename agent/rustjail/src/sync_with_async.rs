// Copyright (c) 2020 Ant Group
//
// SPDX-License-Identifier: Apache-2.0
//

//! The async version of sync module used for IPC

use crate::pipestream::PipeStream;
use anyhow::{anyhow, Result};
use nix::errno::Errno;
use tokio::io::{AsyncReadExt, AsyncWriteExt};

use crate::sync::{DATA_SIZE, MSG_SIZE, SYNC_DATA, SYNC_FAILED, SYNC_SUCCESS};

async fn write_count(pipe_w: &mut PipeStream, buf: &[u8], count: usize) -> Result<usize> {
    let mut len = 0;

    loop {
        match pipe_w.write(&buf[len..]).await {
            Ok(l) => {
                len += l;
                if len == count {
                    break;
                }
            }

            Err(e) => {
                if e.raw_os_error().unwrap() != Errno::EINTR as i32 {
                    return Err(e.into());
                }
            }
        }
    }

    Ok(len)
}

async fn read_count(pipe_r: &mut PipeStream, count: usize) -> Result<Vec<u8>> {
    let mut v: Vec<u8> = vec![0; count];
    let mut len = 0;

    loop {
        match pipe_r.read(&mut v[len..]).await {
            Ok(l) => {
                len += l;
                if len == count || l == 0 {
                    break;
                }
            }

            Err(e) => {
                if e.raw_os_error().unwrap() != Errno::EINTR as i32 {
                    return Err(e.into());
                }
            }
        }
    }

    Ok(v[0..len].to_vec())
}

pub async fn read_async(pipe_r: &mut PipeStream) -> Result<Vec<u8>> {
    let buf = read_count(pipe_r, MSG_SIZE).await?;
    if buf.len() != MSG_SIZE {
        return Err(anyhow!(
            "process: {} failed to receive async message from peer: got msg length: {}, expected: {}",
            std::process::id(),
            buf.len(),
            MSG_SIZE
        ));
    }
    let buf_array: [u8; MSG_SIZE] = [buf[0], buf[1], buf[2], buf[3]];
    let msg: i32 = i32::from_be_bytes(buf_array);
    match msg {
        SYNC_SUCCESS => Ok(Vec::new()),
        SYNC_DATA => {
            let buf = read_count(pipe_r, MSG_SIZE).await?;
            let buf_array: [u8; MSG_SIZE] = [buf[0], buf[1], buf[2], buf[3]];
            let msg_length: i32 = i32::from_be_bytes(buf_array);
            let data_buf = read_count(pipe_r, msg_length as usize).await?;

            Ok(data_buf)
        }
        SYNC_FAILED => {
            let mut error_buf = vec![];
            loop {
                let buf = read_count(pipe_r, DATA_SIZE).await?;

                error_buf.extend(&buf);
                if DATA_SIZE == buf.len() {
                    continue;
                } else {
                    break;
                }
            }

            let error_str = match std::str::from_utf8(&error_buf) {
                Ok(v) => String::from(v),
                Err(e) => {
                    return Err(
                        anyhow!(e).context("receive error message from child process failed")
                    );
                }
            };

            Err(anyhow!(error_str))
        }
        _ => Err(anyhow!("error in receive sync message")),
    }
}

pub async fn write_async(pipe_w: &mut PipeStream, msg_type: i32, data_str: &str) -> Result<()> {
    let buf = msg_type.to_be_bytes();
    let count = write_count(pipe_w, &buf, MSG_SIZE).await?;
    if count != MSG_SIZE {
        return Err(anyhow!("error in send sync message"));
    }

    match msg_type {
        SYNC_FAILED => {
            if let Err(e) = write_count(pipe_w, data_str.as_bytes(), data_str.len()).await {
                return Err(anyhow!(e).context("error in send message to process"));
            }
        }
        SYNC_DATA => {
            let length: i32 = data_str.len() as i32;
            write_count(pipe_w, &length.to_be_bytes(), MSG_SIZE)
                .await
                .map_err(|e| anyhow!(e).context("error in send message to process"))?;

            write_count(pipe_w, data_str.as_bytes(), data_str.len())
                .await
                .map_err(|e| anyhow!(e).context("error in send message to process"))?;
        }

        _ => (),
    };

    Ok(())
}
