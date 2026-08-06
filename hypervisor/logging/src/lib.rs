use slog::*;
use slog_term::{Decorator, RecordDecorator};
use std::cell::RefCell;
use std::io::Write;
use std::sync::Mutex;
use std::{io, result};

#[macro_use]
extern crate lazy_static;

/// Slog tag used to request an explicit log-file reopen.
pub const LOG_CTRL_REOPEN: &str = "LogReopen";

fn open_log_file(name: &str) -> io::Result<std::fs::File> {
    std::fs::File::options()
        .create(true)
        .append(true)
        .open(std::path::Path::new(name))
}

fn open_replacement_log_file(name: &str) -> io::Result<std::fs::File> {
    std::fs::File::options()
        .create(true)
        .append(true)
        .open(std::path::Path::new(name))
}

lazy_static! {
    pub static ref START_TM: Mutex<std::time::Instant> = Mutex::new(std::time::Instant::now());
}

// {{{ Plain

pub struct RawPlainDecorator {
    name: String,
    output: std::fs::File,
}

pub struct PlainDecorator {
    deco: RefCell<RawPlainDecorator>,
}

impl PlainDecorator {
    /// Create `PlainDecorator` instance
    pub fn new(name: String) -> Self {
        let filename = name.clone();
        let output = open_log_file(&filename).unwrap();
        PlainDecorator {
            deco: RefCell::new(RawPlainDecorator { name, output }),
        }
    }
}

impl Decorator for PlainDecorator {
    fn with_record<F>(&self, record: &Record, _logger_values: &OwnedKVList, f: F) -> io::Result<()>
    where
        F: FnOnce(&mut dyn RecordDecorator) -> io::Result<()>,
    {
        let mut deco = self.deco.borrow_mut();
        if record.tag() == LOG_CTRL_REOPEN {
            if let Err(error) = deco.reopen() {
                eprintln!(
                    "cube-vmm: failed to reopen log file {:?}: {}",
                    deco.name, error
                );
            }
            return Ok(());
        }
        let mut deco = PlainRecordDecorator { deco: &mut *deco };
        f(&mut deco)
    }
}

/// Record decorator used by `PlainDecorator`
pub struct PlainRecordDecorator<'a> {
    deco: &'a mut RawPlainDecorator,
}

impl RawPlainDecorator {
    fn reopen(&mut self) -> io::Result<()> {
        self.output.flush()?;
        let output = open_replacement_log_file(&self.name)?;
        self.output = output;
        Ok(())
    }
}

impl<'a> io::Write for PlainRecordDecorator<'a> {
    fn write(&mut self, buf: &[u8]) -> io::Result<usize> {
        self.deco.output.write(buf)
    }

    fn flush(&mut self) -> io::Result<()> {
        self.deco.output.flush()
    }
}

impl<'a> RecordDecorator for PlainRecordDecorator<'a> {
    fn reset(&mut self) -> io::Result<()> {
        Ok(())
    }
}

//// {{{ RawFormat

pub struct RawFormat<D>
where
    D: Decorator,
{
    decorator: D,
}

/// Streamer builder
pub struct RawFormatBuilder<D>
where
    D: Decorator,
{
    decorator: D,
}

impl<D> RawFormatBuilder<D>
where
    D: Decorator,
{
    /// Build the streamer
    pub fn build(self) -> RawFormat<D> {
        RawFormat {
            decorator: self.decorator,
        }
    }
}

impl<D> Drain for RawFormat<D>
where
    D: Decorator,
{
    type Ok = ();
    type Err = io::Error;

    fn log(&self, record: &Record, values: &OwnedKVList) -> result::Result<Self::Ok, Self::Err> {
        self.format_compact(record, values)
    }
}

impl<D> RawFormat<D>
where
    D: Decorator,
{
    /// New `RawFormatBuilder`
    #[allow(clippy::new_ret_no_self)]
    pub fn new(d: D) -> RawFormatBuilder<D> {
        RawFormatBuilder { decorator: d }
    }

    fn format_compact(&self, record: &Record, values: &OwnedKVList) -> io::Result<()> {
        self.decorator.with_record(record, values, |mut decorator| {
            decorator.start_msg()?;
            let mut count_rd = slog_term::CountingWriter::new(&mut decorator);
            write!(count_rd, "{}", record.msg())?;

            decorator.flush()?;

            Ok(())
        })
    }
}

pub fn create_logger(name: String) -> slog::Logger {
    let decorator = PlainDecorator::new(name);
    let drain = RawFormat::new(decorator).build().ignore_res();
    let drain = slog_async::Async::new(drain)
        .chan_size(8192)
        .thread_name("cube-log".to_string())
        .overflow_strategy(slog_async::OverflowStrategy::Drop)
        .build()
        .ignore_res();
    let logger = slog::Logger::root(drain, o!());

    logger
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;
    use std::sync::atomic::{AtomicU64, Ordering};
    use std::time::{SystemTime, UNIX_EPOCH};

    static NEXT_TEST_ID: AtomicU64 = AtomicU64::new(0);

    fn test_log_path() -> std::path::PathBuf {
        let suffix = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("system clock before unix epoch")
            .as_nanos();
        let test_id = NEXT_TEST_ID.fetch_add(1, Ordering::Relaxed);
        std::env::temp_dir().join(format!(
            "cube-vmm-log-reopen-{}-{}-{}",
            std::process::id(),
            suffix,
            test_id
        ))
    }

    struct TestLogFiles {
        active: std::path::PathBuf,
        rotated: std::path::PathBuf,
    }

    impl TestLogFiles {
        fn new() -> Self {
            let active = test_log_path();
            let rotated = active.with_extension("log.1");
            Self { active, rotated }
        }
    }

    impl Drop for TestLogFiles {
        fn drop(&mut self) {
            let _ = fs::remove_file(&self.active);
            let _ = fs::remove_file(&self.rotated);
        }
    }

    #[test]
    fn control_record_reopens_after_rename_based_rotation() {
        let files = TestLogFiles::new();
        let drain = RawFormat::new(PlainDecorator::new(
            files.active.to_string_lossy().into_owned(),
        ))
        .build();
        let values = OwnedKVList::from(o!());

        drain
            .log(
                &slog::record!(Level::Info, "", &format_args!("before"), b!()),
                &values,
            )
            .unwrap();

        fs::rename(&files.active, &files.rotated).unwrap();
        fs::File::create(&files.active).unwrap();

        drain
            .log(
                &slog::record!(Level::Info, LOG_CTRL_REOPEN, &format_args!(""), b!()),
                &values,
            )
            .unwrap();
        drain
            .log(
                &slog::record!(Level::Info, "", &format_args!("after"), b!()),
                &values,
            )
            .unwrap();

        assert_eq!(fs::read_to_string(&files.rotated).unwrap(), "before");
        assert_eq!(fs::read_to_string(&files.active).unwrap(), "after");
    }

    #[test]
    fn failed_reopen_keeps_current_descriptor_usable() {
        let files = TestLogFiles::new();
        let output = open_log_file(files.active.to_str().expect("utf-8 log path")).unwrap();
        let mut raw = RawPlainDecorator {
            name: std::env::temp_dir().to_string_lossy().into_owned(),
            output,
        };

        raw.output.write_all(b"before\n").unwrap();
        assert!(raw.reopen().is_err());
        raw.output.write_all(b"after\n").unwrap();

        assert_eq!(
            fs::read_to_string(&files.active).unwrap(),
            "before\nafter\n"
        );
    }
}
