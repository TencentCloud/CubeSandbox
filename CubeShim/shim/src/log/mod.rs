// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

pub mod stat_defer;
use crate::common::CResult;
use nix::unistd::dup2;
use serde::{Deserialize, Serialize};
use serde_json;

use std::io;
use std::io::Write;
use std::mem;
use std::os::unix::io::AsRawFd;
use std::path::PathBuf;

use std::time::SystemTime;
use time::format_description::well_known::Rfc3339;
use time::OffsetDateTime;
use tokio::fs::OpenOptions;
use tokio::io::AsyncWriteExt;
use tokio::net::UnixDatagram;

use tokio::sync::mpsc::{self, Receiver, Sender};
use tokio::time::{sleep, Duration};

#[macro_export]
macro_rules! debugf {
    ($log:expr, $($arg:tt)*) => {{
        let msg = format!($($arg)*);
        let _ = $log.debug(msg);
    }};
}

#[macro_export]
macro_rules! infof {
    ($log:expr, $($arg:tt)*) => {{
        let msg = format!($($arg)*);
        let _ = $log.info(msg);
    }};
}

#[macro_export]
macro_rules! warnf {
    ($log:expr, $($arg:tt)*) => {{
        let msg = format!($($arg)*);
        let _ = $log.warn(msg);
    }};
}

#[macro_export]
macro_rules! errf {
    ($log:expr, $($arg:tt)*) => {{
        let msg = format!($($arg)*);
        let _ = $log.error(msg);
    }};
}

const LOG_ITEM_COUNT: usize = 1024;
const LOG_DIR: &str = "/data/log/CubeShim/";
const LOF_FILE: &str = "cube-shim-req.log";
const STAT_FILE: &str = "cube-shim-stat.log";
const ENV_FUNCTION_TYPE: &str = "FUNCTION_TYPE";
#[derive(Clone, PartialEq, PartialOrd)]
pub enum LogLevel {
    Debug,
    Info,
    Warn,
    Error,
}

enum LogType {
    Log,
    Stat,
    Rotate,
}

#[derive(Clone, Serialize, Deserialize, Debug)]
pub enum StatRet {
    Ok,
    Err,
}

#[derive(Clone)]
pub struct Log {
    module: String,
    instance_id: String,
    container_id: String,
    sender: Sender<(LogType, String)>,
    level: LogLevel,
    function_type: String,
}

#[derive(Serialize, Deserialize, Debug)]
#[serde(rename_all = "PascalCase")]
struct LogItem {
    module: String,
    instance_id: String,
    container_id: String,
    timestamp: String,
    log_content: String,
    function_type: String,
}

#[derive(Serialize, Deserialize, Debug)]
#[serde(rename_all = "PascalCase")]
struct StatItem {
    module: String,
    instance_id: String,
    container_id: String,
    caller: String,
    action: String,
    callee: String,
    callee_action: String,
    ret_code: StatRet,
    cost_time: u128,
    function_type: String,
}

impl Default for Log {
    fn default() -> Self {
        let (sender, _) = mpsc::channel::<(LogType, String)>(1);
        Log {
            sender,
            module: String::new(),
            instance_id: String::new(),
            container_id: String::new(),
            level: LogLevel::Info,
            function_type: std::env::var(ENV_FUNCTION_TYPE).unwrap_or_default(),
        }
    }
}

impl Log {
    pub fn new(id: String, module: String, level: LogLevel) -> Self {
        let (sender, receiver) = mpsc::channel::<(LogType, String)>(LOG_ITEM_COUNT);
        let log = Log {
            module: module.clone(),
            instance_id: id.clone(),
            container_id: id.clone(),
            sender: sender.clone(),
            level,
            function_type: std::env::var(ENV_FUNCTION_TYPE).unwrap_or_default(),
        };
        let _ = std::fs::create_dir_all(LOG_DIR);

        let (reader, writer) = UnixDatagram::pair().unwrap();

        // Only take over stderr here. Do NOT dup2 over stdout (fd 1): in the shim
        // daemon, fd 1 is the readiness pipe that containerd-shim's parent `start`
        // process copies until EOF. Closing it before the ttrpc server has bound and
        // started listening makes the parent return the socket address too early, so
        // containerd dials a not-yet-bound socket and fails with
        // "failed to create TTRPC connection: ... connect: no such file or directory".
        // The crate's signal_server_started() runs dup2(STDERR->STDOUT) only after
        // server.start(), which then redirects stdout onto this datagram for us while
        // preserving the readiness handshake.
        dup2(writer.as_raw_fd(), std::io::stderr().as_raw_fd()).expect("dup stderr failed");
        mem::forget(writer);

        tokio::spawn(Self::consumer(sender.clone(), receiver));
        tokio::spawn(Self::forward(sender.clone(), reader, log.clone()));

        let panic_mod = module.clone();
        let panic_insid = id.clone();
        let panic_ft = log.function_type.clone();
        std::panic::set_hook(Box::new(move |panic_info| {
            if let Err(e) = log_to_file(
                panic_mod.clone(),
                panic_insid.clone(),
                format!("Panic:{:?}", panic_info),
                panic_ft.clone(),
            ) {
                eprintln!("log panic info failed:{:?} {:?}", panic_info, e);
            }
            std::process::exit(-1);
        }));
        log
    }
    async fn forward(_sender: Sender<(LogType, String)>, reader: UnixDatagram, log: Log) {
        let mut buffer = String::new();
        let mut counter = 0;
        let qos_quota = 1000;
        let qos_period = std::time::Duration::from_secs(3600);
        let mut tm = SystemTime::now() + qos_period;
        loop {
            let mut buf = [0; 1024];
            match reader.recv(&mut buf).await {
                Ok(n) => {
                    if n == 0 {
                        errf!(log, "forward log failed, the peer is closed");
                        return;
                    }

                    let mut bufs = String::from_utf8_lossy(&buf[..n]).to_string();

                    loop {
                        if counter >= qos_quota && SystemTime::now() >= tm {
                            counter = 0;
                            tm = SystemTime::now() + qos_period;
                        }

                        if let Some((l, r)) = bufs.split_once('\n') {
                            buffer.push_str(l);
                            if counter < qos_quota {
                                log.info(buffer.clone());
                                counter += 1;
                            }
                            buffer.clear();
                            bufs = r.to_string();
                        } else {
                            buffer.push_str(&bufs);
                            if buffer.len() > 4096 {
                                if counter < qos_quota {
                                    log.info(buffer.clone());
                                    counter += 1;
                                }

                                buffer.clear();
                            }
                            break;
                        }
                    }
                }
                Err(ref e) if e.kind() == io::ErrorKind::BrokenPipe => {
                    errf!(log, "forward log failed read log error:{}", e);
                    return;
                }
                Err(e) => {
                    errf!(log, "forward log error:{}", e);
                    continue;
                }
            }
        }
    }
    async fn consumer(send: Sender<(LogType, String)>, mut recv: Receiver<(LogType, String)>) {
        let mut log_file: PathBuf = PathBuf::from(LOG_DIR);
        log_file.push(LOF_FILE);

        let mut stat_file = PathBuf::from(LOG_DIR);
        stat_file.push(STAT_FILE);

        tokio::spawn(async move {
            loop {
                let ret: Result<(), String> =
                    Self::write_log_rotate(&mut recv, &log_file, &stat_file).await;
                if let Err(e) = ret {
                    eprintln!("write log failed:{}", e);
                    sleep(Duration::from_secs(3)).await;
                }
            }
        });

        loop {
            sleep(Duration::from_secs(1800)).await;
            if let Err(e) = send.send((LogType::Rotate, "".to_string())).await {
                eprintln!("send rotate failed:{}", e);
            }
        }
    }

    async fn write_log_rotate(
        recv: &mut Receiver<(LogType, String)>,
        log_file_path: &PathBuf,
        stat_file_path: &PathBuf,
    ) -> CResult<()> {
        let log_file = OpenOptions::new()
            .create(true)
            .write(true)
            .append(true)
            .open(log_file_path.clone())
            .await
            .map_err(|e| format!("open log file failed:{} file:{:?}", e, log_file_path))?;
        let mut log_writer = tokio::io::BufReader::new(log_file);

        let stat_file = OpenOptions::new()
            .create(true)
            .write(true)
            .append(true)
            .open(stat_file_path.clone())
            .await
            .map_err(|e| format!("open stat file failed:{} file:{:?}", e, stat_file_path))?;
        let mut stat_writer = tokio::io::BufReader::new(stat_file);

        //let lf = ['\n' as u8];
        while let Some(msg) = recv.recv().await {
            match msg.0 {
                LogType::Log => {
                    log_writer
                        .write_all(msg.1.as_bytes())
                        .await
                        .map_err(|e| format!("write log file failed:{}", e))?;
                    //log_writer.write_all(&lf).await.map_err(|e| format!("write log file failed:{}", e));
                    log_writer
                        .flush()
                        .await
                        .map_err(|e| format!("flush log failed:{}", e))?;
                }
                LogType::Stat => {
                    stat_writer
                        .write_all(msg.1.as_bytes())
                        .await
                        .map_err(|e| format!("write stat file failed:{}", e))?;
                    //stat_writer.write_all(&lf).await.map_err(|e| format!("write stat file failed:{}", e));
                    stat_writer
                        .flush()
                        .await
                        .map_err(|e| format!("flush stat failed:{}", e))?;
                }
                LogType::Rotate => {
                    break;
                }
            }
        }
        Ok(())
    }

    pub fn set_container_id(&mut self, id: String) {
        self.container_id = id;
    }

    fn log(&self, log: String) {
        let now = SystemTime::now();
        let datetime = OffsetDateTime::from(now);
        let li = LogItem {
            module: self.module.clone(),
            instance_id: self.instance_id.clone(),
            container_id: self.container_id.clone(),
            timestamp: datetime.format(&Rfc3339).unwrap_or_default(),
            log_content: log,
            function_type: self.function_type.clone(),
        };

        let msg_ret = serde_json::to_string(&li);
        match msg_ret {
            Ok(msg) => {
                let _ = self.sender.try_send((LogType::Log, msg + "\n"));
            }
            Err(e) => {
                println!("Serialize LogItem failed:{}", e)
            }
        }
    }

    pub fn debug(&self, log: String) {
        if self.level > LogLevel::Debug {
            return;
        }
        self.log(log)
    }

    pub fn info(&self, log: String) {
        if self.level > LogLevel::Info {
            return;
        }
        self.log(log)
    }

    pub fn warn(&self, log: String) {
        if self.level > LogLevel::Warn {
            return;
        }
        self.log(log)
    }

    pub fn error(&self, log: String) {
        if self.level > LogLevel::Error {
            return;
        }
        self.log(log)
    }

    #[allow(clippy::too_many_arguments)]
    pub fn stat(
        &self,
        container_id: String,
        callee: String,
        action: String,
        callee_action: String,
        ret: StatRet,
        cost: u128,
    ) {
        let si = StatItem {
            module: self.module.clone(),
            instance_id: self.instance_id.clone(),
            container_id,
            caller: self.module.clone(),
            callee,
            callee_action,
            action,
            ret_code: ret,
            cost_time: cost,
            function_type: self.function_type.clone(),
        };

        let msg_ret = serde_json::to_string(&si);
        match msg_ret {
            Ok(msg) => {
                let _ = self.sender.try_send((LogType::Stat, msg + "\n"));
            }
            Err(e) => {
                println!("Serialize StatItem failed:{}", e)
            }
        }
    }
}

fn log_to_file(module: String, insid: String, log: String, func_type: String) -> CResult<()> {
    let now = SystemTime::now();
    let datetime = OffsetDateTime::from(now);
    let li = LogItem {
        module: module,
        instance_id: insid,
        container_id: "".to_string(),
        timestamp: datetime.format(&Rfc3339).unwrap_or_default(),
        log_content: log,
        function_type: func_type,
    };

    let mut msg = serde_json::to_string(&li).map_err(|e| format!("format log failed:{}", e))?;
    msg = msg + "\n";

    let mut log_file: PathBuf = PathBuf::from(LOG_DIR);
    log_file.push(LOF_FILE);
    let mut logf = std::fs::OpenOptions::new()
        .create(true)
        .write(true)
        .append(true)
        .open(log_file.clone())
        .map_err(|e| format!("open log file failed:{} file:{:?}", e, log_file))?;

    logf.write_all(msg.as_bytes())
        .map_err(|e| format!("write file failed:{:?} file:{:?}", e, log_file))?;

    Ok(())
}
