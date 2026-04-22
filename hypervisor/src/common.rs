// Copyright © 2024 Tencent Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

use std::io::Write;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};

use arc_swap::ArcSwap;
use lazy_static::lazy_static;
use logging::LOG_CTRL_REOPEN;
use slog::slog_info;

pub const DEFAULT_LOG_FILE: &str = "/data/log/CubeVmm/vmm.log";
#[allow(dead_code)]
pub const DEFAULT_LOG_JSON_FILE: &str = "/data/log/CubeVmm/vmm.json";

#[allow(dead_code)]
pub fn default_coredump_filter() -> String {
    "0x33".to_string()
}

#[allow(dead_code)]
pub fn default_coredump_limit() -> u64 {
    2 * 1024 * 1024 * 1024
}

pub const DEFAULT_LOGGER_BUFFER_SIZE: usize = 100;

lazy_static! {
    static ref LOG_GUARD: ArcSwap<Option<slog_scope::GlobalLoggerGuard>> =
        ArcSwap::from(Arc::new(None));
}

pub struct Logger {
    output: Mutex<Option<Box<dyn std::io::Write + Send>>>,
    start: std::time::Instant,
    sandbox_id: String,
    log_async: bool,
    defer_logger_thread: bool,
    buffer: Arc<Mutex<Vec<String>>>,
    logger_thread_started: Arc<Mutex<AtomicBool>>,
    log_file_name: String,
    vcpu_started: Arc<AtomicBool>,
    need_flush: Arc<AtomicBool>,
    #[cfg(feature = "logger_debug")]
    logger_dbg_file: Arc<Mutex<File>>,
}

impl Logger {
    #[allow(clippy::too_many_arguments)]
    pub fn new(
        output: Mutex<Option<Box<dyn std::io::Write + Send>>>,
        start: std::time::Instant,
        sandbox_id: String,
        log_async: bool,
        defer_logger_thread: bool,
        buffer: Arc<Mutex<Vec<String>>>,
        logger_thread_started: Arc<Mutex<AtomicBool>>,
        log_file_name: String,
        vcpu_started: Arc<AtomicBool>,
        need_flush: Arc<AtomicBool>,
        #[cfg(feature = "logger_debug")] logger_dbg_file: Arc<Mutex<File>>,
    ) -> Self {
        Self {
            output,
            start,
            sandbox_id,
            log_async,
            defer_logger_thread,
            buffer,
            logger_thread_started,
            log_file_name,
            vcpu_started,
            need_flush,
            #[cfg(feature = "logger_debug")]
            logger_dbg_file,
        }
    }
    fn log_async_flush(&self, buf: &[u8]) {
        let mut output = self.output.lock().unwrap();
        if output.is_none() {
            *output = match std::fs::File::options()
                .create(true)
                .append(true)
                .open(std::path::Path::new(&self.log_file_name))
            {
                Ok(file) => Some(Box::new(file)),
                Err(_) => Some(Box::new(std::io::stderr())),
            };
        }
        if output.is_some() {
            (*(output.as_mut().unwrap())).write(buf).ok();
        }
    }

    fn build_logger_thread(&self) {
        let logger = logging::create_logger(self.log_file_name.clone());
        let guard = Some(slog_scope::set_global_logger(logger));
        LOG_GUARD.store(Arc::new(guard));
    }

    fn log_async(&self, data: String, target: &str) {
        #[cfg(feature = "logger_debug")]
        let _ = self
            .logger_dbg_file
            .lock()
            .unwrap()
            .write_all(data.as_bytes());

        if self
            .logger_thread_started
            .lock()
            .unwrap()
            .load(Ordering::SeqCst)
        {
            match target {
                LOG_CTRL_REOPEN => slog_info!(slog_scope::logger(), #LOG_CTRL_REOPEN, "{}", data),
                _ => slog_info!(slog_scope::logger(), "{}", data),
            }
            return;
        }

        if self.need_flush.load(Ordering::SeqCst) {
            self.log_async_flush(data.as_bytes());
        }

        if !self.defer_logger_thread {
            let logger_started = self.logger_thread_started.lock().unwrap();
            if !logger_started.load(Ordering::SeqCst) {
                self.build_logger_thread();
                match target {
                    LOG_CTRL_REOPEN => {
                        slog_info!(slog_scope::logger(), #LOG_CTRL_REOPEN, "{}", data)
                    }
                    _ => slog_info!(slog_scope::logger(), "{}", data),
                };
                logger_started.store(true, Ordering::SeqCst);
            } else {
                // logger thread has been started, slog those waiting for
                // logger_thread_started lock
                match target {
                    LOG_CTRL_REOPEN => {
                        slog_info!(slog_scope::logger(), #LOG_CTRL_REOPEN, "{}", data)
                    }
                    _ => slog_info!(slog_scope::logger(), "{}", data),
                };
            }

            return;
        }

        if self.vcpu_started.load(Ordering::SeqCst) {
            let logger_started = self.logger_thread_started.lock().unwrap();
            if !logger_started.load(Ordering::SeqCst) {
                self.build_logger_thread();
                for v in self.buffer.lock().unwrap().drain(..) {
                    slog_info!(slog_scope::logger(), "{}", v);
                }
                self.buffer.lock().unwrap().shrink_to_fit();
                // log this info/debug/warn, 'cause this
                // data has not been pushed into buffer
                match target {
                    LOG_CTRL_REOPEN => {
                        slog_info!(slog_scope::logger(), #LOG_CTRL_REOPEN, "{}", data)
                    }
                    _ => slog_info!(slog_scope::logger(), "{}", data),
                };
                logger_started.store(true, Ordering::SeqCst);
            } else {
                // logger thread has been started, slog those waiting for
                // logger_thread_started lock
                match target {
                    LOG_CTRL_REOPEN => {
                        slog_info!(slog_scope::logger(), #LOG_CTRL_REOPEN, "{}", data)
                    }
                    _ => slog_info!(slog_scope::logger(), "{}", data),
                };
            }
        } else {
            self.buffer.lock().unwrap().push(data);
        }
    }

    fn log_flush(&self) {
        if !self.log_async {
            return;
        }

        for v in self.buffer.lock().unwrap().drain(..) {
            self.log_async_flush(v.as_bytes());
        }
        self.need_flush.store(true, Ordering::SeqCst);
    }
}

impl log::Log for Logger {
    fn enabled(&self, _metadata: &log::Metadata) -> bool {
        true
    }

    fn log(&self, record: &log::Record) {
        if !self.enabled(record.metadata()) {
            return;
        }

        // Drop control log under sync mode.
        if !self.log_async && record.target() == LOG_CTRL_REOPEN {
            return;
        }

        let now = std::time::Instant::now();
        let duration = now.duration_since(self.start);

        if record.file().is_some() && record.line().is_some() {
            let t = format!(
                "{} --- {:?} --- {} --- <{}> {}:{} -- {}\n",
                self.sandbox_id,
                duration.as_millis(),
                record.level(),
                std::thread::current().name().unwrap_or("anonymous"),
                record.file().unwrap(),
                record.line().unwrap(),
                record.args(),
            );
            if self.log_async {
                self.log_async(t, record.target());
            } else {
                (*(self.output.lock().unwrap().as_mut().unwrap()))
                    .write(t.as_bytes())
                    .ok();
            }
        } else {
            let t = format!(
                "{} --- {:?} --- {} --- <{}> {} -- {}\n",
                self.sandbox_id,
                duration.as_millis(),
                record.level(),
                std::thread::current().name().unwrap_or("anonymous"),
                record.target(),
                record.args(),
            );
            if self.log_async {
                self.log_async(t, record.target());
            } else {
                (*(self.output.lock().unwrap().as_mut().unwrap()))
                    .write(t.as_bytes())
                    .ok();
            }
        }
    }

    fn flush(&self) {
        self.log_flush();
    }
}
