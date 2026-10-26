//! Copyright (c) 2024 Tencent Cloud
use std::{
    fs::File,
    os::fd::{AsRawFd, RawFd},
    path::Path,
    sync::Once,
    time::{SystemTime, UNIX_EPOCH},
};

use libc::{clock_settime, timespec, CLOCK_REALTIME, PTP_SYS_OFFSET};
use nix::sys::time::TimeSpec;

/// 单次调整的最大3ms
const MAX_ADJUST_NS: u64 = 3_000_000;

/// 默认的时钟同步周期，单位为秒
const DEFAULT_TIME_SYNC_PERIOD: tokio::time::Duration = tokio::time::Duration::from_secs(30);
/// 默认的时钟同步最小偏移量，单位为纳秒，当时间偏移大于该值时，会触发内核时钟同步
const DEFAULT_TIME_SYNC_OFFSET_MIN_NS: u64 = 1_000_000;

const PTP_DEVICE_PATH: &str = "/dev/ptp0";

// Convenience macro to obtain the scope logger
macro_rules! sl {
    () => {
        slog_scope::logger().new(o!("subsystem" => "time_sync"))
    };
}

#[repr(C)]
#[derive(Default, Clone, Copy, Debug, PartialEq, Eq)]
pub struct PtpClockTime {
    pub seconds: i64,
    pub nanoseconds: u32,
    pub reserved: u32,
}

impl Into<TimeSpec> for PtpClockTime {
    fn into(self) -> TimeSpec {
        TimeSpec::from_timespec(timespec {
            tv_sec: self.seconds,
            tv_nsec: self.nanoseconds as i64,
        })
    }
}

const PTP_MAX_SAMPLES: usize = 25;
const PTP_SAMPLES: usize = 3;

#[repr(C)]
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct PtpSysOffset {
    pub n_samples: u32,
    pub rsv: [u32; 3],
    pub ts: [PtpClockTime; 2 * PTP_MAX_SAMPLES + 1],
}

impl Default for PtpSysOffset {
    fn default() -> Self {
        PtpSysOffset {
            n_samples: 0,
            rsv: [0; 3],
            ts: [PtpClockTime::default(); 2 * PTP_MAX_SAMPLES + 1],
        }
    }
}

#[derive(Debug, Clone, Copy)]
struct PtpTimeStampSample {
    sys_time_0: TimeSpec,
    ptp_time: TimeSpec,
    sys_time_1: TimeSpec,
    offset_ns: i64,
}

impl Default for PtpTimeStampSample {
    fn default() -> Self {
        let zero_time_spec = TimeSpec::from_timespec(timespec {
            tv_sec: 0,
            tv_nsec: 0,
        });
        PtpTimeStampSample {
            sys_time_0: zero_time_spec,
            ptp_time: zero_time_spec,
            sys_time_1: zero_time_spec,
            offset_ns: 0,
        }
    }
}

impl PtpTimeStampSample {
    fn calculate_offset(&mut self) {
        let sys_time_avg = self.sys_time_0 + (self.sys_time_1 - self.sys_time_0) / 2;
        let delta = sys_time_avg - self.ptp_time;
        self.offset_ns = delta.tv_nsec() + delta.tv_sec() * 1_000_000_000;
    }
}

fn step_adjust_system_time(offset_ns: i64) -> anyhow::Result<()> {
    // 获取当前系统时间
    let current_time = SystemTime::now();
    let duration_since_epoch = current_time.duration_since(UNIX_EPOCH)?;

    // 计算调整后的时间
    let adjusted_duration =
        duration_since_epoch + std::time::Duration::from_nanos(offset_ns as u64);

    // 将调整后的时间转换为 timespec 结构体
    let ts = timespec {
        tv_sec: adjusted_duration.as_secs() as i64,
        tv_nsec: adjusted_duration.subsec_nanos() as i64,
    };

    // 使用 clock_settime 设置系统时间
    unsafe {
        if clock_settime(CLOCK_REALTIME, &ts) != 0 {
            let err = nix::errno::Errno::last();
            error!(
                sl!(),
                "step_adjust_system_time: Failed to set system time, err: {:?}", err
            );
        }
    }

    Ok(())
}

fn sample_ptp(
    ptpfd: RawFd,
    samples_buf: &mut [PtpTimeStampSample; PTP_SAMPLES],
) -> anyhow::Result<()> {
    let mut sys_off: PtpSysOffset = PtpSysOffset::default();
    sys_off.n_samples = PTP_SAMPLES as u32;

    let r = unsafe { libc::ioctl(ptpfd, PTP_SYS_OFFSET.try_into().unwrap(), &mut sys_off) };
    if r < 0 {
        return Err(anyhow::anyhow!("sample_ptp: Ioctl Error: {}", r));
    }

    for i in 0..sys_off.n_samples as usize {
        let mut sample = PtpTimeStampSample::default();
        sample.sys_time_0 = sys_off.ts[i * 2].into();
        sample.ptp_time = sys_off.ts[i * 2 + 1].into();
        sample.sys_time_1 = sys_off.ts[i * 2 + 2].into();
        sample.calculate_offset();
        samples_buf[i] = sample;
    }

    Ok(())
}

async fn time_sync_task() {
    let period = DEFAULT_TIME_SYNC_PERIOD;
    let mut offset_min_ns = DEFAULT_TIME_SYNC_OFFSET_MIN_NS;
    let mut sample_buf = [PtpTimeStampSample::default(); PTP_SAMPLES];
    if offset_min_ns > MAX_ADJUST_NS {
        offset_min_ns = MAX_ADJUST_NS;
    }

    info!(
        sl!(),
        "time_sync_task: started. period: {}ms, adjust threshold: {}ms, max adjust: {}ms",
        period.as_millis(),
        offset_min_ns / 1_000_000,
        MAX_ADJUST_NS / 1_000_000
    );

    let path = Path::new(PTP_DEVICE_PATH);
    if !path.exists() {
        error!(
            sl!(),
            "time_sync_task: /dev/ptp0 not found, time sync disabled"
        );
        return;
    }
    let file = match File::open(path) {
        Ok(f) => f,
        Err(e) => {
            error!(sl!(), "sample_ptp: Failed to open /dev/ptp0: {}", e);
            return;
        }
    };

    loop {
        tokio::time::sleep(period).await;
        if let Err(e) = sample_ptp(file.as_raw_fd(), &mut sample_buf) {
            error!(sl!(), "time_sync_task: Failed to sample ptp: {}", e);
            continue;
        }

        let avg_offset_ns =
            sample_buf.iter().map(|s| s.offset_ns).sum::<i64>() / sample_buf.len() as i64;

        if avg_offset_ns < 0 && (avg_offset_ns.abs() as u64 >= offset_min_ns) {
            // 子机时间落后了

            // 如果子机时间慢了超过1s，打印一个警告
            if avg_offset_ns.abs() > 1_000_000_000 {
                warn!(
                    sl!(),
                    "time_sync_task: system time behind ptp time: {}ms",
                    avg_offset_ns.abs() / 1000_000
                );
            }

            let adjust_ns = std::cmp::min(avg_offset_ns.abs(), MAX_ADJUST_NS as i64);
            if let Err(e) = step_adjust_system_time(adjust_ns) {
                error!(sl!(), "time_sync_task: Failed to adjust system time: {}", e);
                continue;
            }
        }
    }
}

static TIME_SYNC_TASK: Once = Once::new();

pub async fn start_time_sync_task() {
    TIME_SYNC_TASK.call_once(|| {
        tokio::spawn(async {
            time_sync_task().await;
        });
    });
}
