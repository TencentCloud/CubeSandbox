// Copyright 2020 Arm Limited (or its affiliates). All rights reserved.
// Copyright 2019 Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

//! ARM PL031 Real Time Clock
//!
//! This module implements a PL031 Real Time Clock (RTC) that provides to provides long time base counter.
//! This is achieved by generating an interrupt signal after counting for a programmed number of cycles of
//! a real-time clock input.
//!
use crate::{read_le_u32, write_le_u32};
use std::fmt;
use std::result;
use std::sync::{Arc, Barrier};
use std::time::Instant;
use vm_device::interrupt::InterruptSourceGroup;
use vm_device::BusDevice;

// As you can see in https://static.docs.arm.com/ddi0224/c/real_time_clock_pl031_r1p3_technical_reference_manual_DDI0224C.pdf
// at section 3.2 Summary of RTC registers, the total size occupied by this device is 0x000 -> 0xFFC + 4 = 0x1000.
// From 0x0 to 0x1C we have following registers:
const RTCDR: u64 = 0x0; // Data Register.
const RTCMR: u64 = 0x4; // Match Register.
const RTCLR: u64 = 0x8; // Load Register.
const RTCCR: u64 = 0xc; // Control Register.
const RTCIMSC: u64 = 0x10; // Interrupt Mask Set or Clear Register.
const RTCRIS: u64 = 0x14; // Raw Interrupt Status.
const RTCMIS: u64 = 0x18; // Masked Interrupt Status.
const RTCICR: u64 = 0x1c; // Interrupt Clear Register.
                          // From 0x020 to 0xFDC => reserved space.
                          // From 0xFE0 to 0x1000 => Peripheral and PrimeCell Identification Registers which are Read Only registers.
                          // AMBA standard devices have CIDs (Cell IDs) and PIDs (Peripheral IDs). The linux kernel will look for these in order to assert the identity
                          // of these devices (i.e look at the `amba_device_try_add` function).
                          // We are putting the expected values (look at 'Reset value' column from above mentioned document) in an array.
const PL031_ID: [u8; 8] = [0x31, 0x10, 0x14, 0x00, 0x0d, 0xf0, 0x05, 0xb1];
// We are only interested in the margins.
const AMBA_ID_LOW: u64 = 0xFE0;
const AMBA_ID_HIGH: u64 = 0x1000;
/// Constant to convert seconds to nanoseconds.
pub const NANOS_PER_SECOND: u64 = 1_000_000_000;

#[derive(Debug)]
pub enum Error {
    BadWriteOffset(u64),
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result {
        match self {
            Error::BadWriteOffset(offset) => write!(f, "Bad Write Offset: {}", offset),
        }
    }
}

type Result<T> = result::Result<T, Error>;

/// Wrapper over `libc::clockid_t` to specify Linux Kernel clock source.
pub enum ClockType {
    /// Equivalent to `libc::CLOCK_MONOTONIC`.
    Monotonic,
    /// Equivalent to `libc::CLOCK_REALTIME`.
    #[allow(dead_code)]
    Real,
    /// Equivalent to `libc::CLOCK_PROCESS_CPUTIME_ID`.
    ProcessCpu,
    /// Equivalent to `libc::CLOCK_THREAD_CPUTIME_ID`.
    #[allow(dead_code)]
    ThreadCpu,
}

impl From<ClockType> for libc::clockid_t {
    fn from(ct: ClockType) -> libc::clockid_t {
        match ct {
            ClockType::Monotonic => libc::CLOCK_MONOTONIC,
            ClockType::Real => libc::CLOCK_REALTIME,
            ClockType::ProcessCpu => libc::CLOCK_PROCESS_CPUTIME_ID,
            ClockType::ThreadCpu => libc::CLOCK_THREAD_CPUTIME_ID,
        }
    }
}

/// Structure representing the date in local time with nanosecond precision.
pub struct LocalTime {
    /// Seconds in current minute.
    sec: i32,
    /// Minutes in current hour.
    min: i32,
    /// Hours in current day, 24H format.
    hour: i32,
    /// Days in current month.
    mday: i32,
    /// Months in current year.
    mon: i32,
    /// Years passed since 1900 BC.
    year: i32,
    /// Nanoseconds in current second.
    nsec: i64,
}

impl LocalTime {
    /// Returns the [LocalTime](struct.LocalTime.html) structure for the calling moment.
    #[allow(dead_code)]
    pub fn now() -> LocalTime {
        let mut timespec = libc::timespec {
            tv_sec: 0,
            tv_nsec: 0,
        };
        let mut tm: libc::tm = libc::tm {
            tm_sec: 0,
            tm_min: 0,
            tm_hour: 0,
            tm_mday: 0,
            tm_mon: 0,
            tm_year: 0,
            tm_wday: 0,
            tm_yday: 0,
            tm_isdst: 0,
            tm_gmtoff: 0,
            tm_zone: std::ptr::null(),
        };

        // Safe because the parameters are valid.
        unsafe {
            libc::clock_gettime(libc::CLOCK_REALTIME, &mut timespec);
            libc::localtime_r(&timespec.tv_sec, &mut tm);
        }

        LocalTime {
            sec: tm.tm_sec,
            min: tm.tm_min,
            hour: tm.tm_hour,
            mday: tm.tm_mday,
            mon: tm.tm_mon,
            year: tm.tm_year,
            nsec: timespec.tv_nsec,
        }
    }
}

impl fmt::Display for LocalTime {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            f,
            "{}-{:02}-{:02}T{:02}:{:02}:{:02}.{:09}",
            self.year + 1900,
            self.mon + 1,
            self.mday,
            self.hour,
            self.min,
            self.sec,
            self.nsec
        )
    }
}

/// Holds a micro-second resolution timestamp with both the real time and cpu time.
#[derive(Clone)]
pub struct TimestampUs {
    /// Real time in microseconds.
    pub time_us: u64,
    /// Cpu time in microseconds.
    pub cputime_us: u64,
}

impl Default for TimestampUs {
    fn default() -> TimestampUs {
        TimestampUs {
            time_us: get_time(ClockType::Monotonic) / 1000,
            cputime_us: get_time(ClockType::ProcessCpu) / 1000,
        }
    }
}

/// Returns a timestamp in nanoseconds from a monotonic clock.
///
/// Uses `_rdstc` on `x86_64` and [`get_time`](fn.get_time.html) on other architectures.
#[allow(dead_code)]
pub fn timestamp_cycles() -> u64 {
    #[cfg(target_arch = "x86_64")]
    // Safe because there's nothing that can go wrong with this call.
    unsafe {
        std::arch::x86_64::_rdtsc() as u64
    }
    #[cfg(not(target_arch = "x86_64"))]
    {
        get_time(ClockType::Monotonic)
    }
}

/// Returns a timestamp in nanoseconds based on the provided clock type.
///
/// # Arguments
///
/// * `clock_type` - Identifier of the Linux Kernel clock on which to act.
pub fn get_time(clock_type: ClockType) -> u64 {
    let mut time_struct = libc::timespec {
        tv_sec: 0,
        tv_nsec: 0,
    };
    // Safe because the parameters are valid.
    unsafe { libc::clock_gettime(clock_type.into(), &mut time_struct) };
    seconds_to_nanoseconds(time_struct.tv_sec).unwrap() as u64 + (time_struct.tv_nsec as u64)
}

/// Converts a timestamp in seconds to an equivalent one in nanoseconds.
/// Returns `None` if the conversion overflows.
///
/// # Arguments
///
/// * `value` - Timestamp in seconds.
pub fn seconds_to_nanoseconds(value: i64) -> Option<i64> {
    value.checked_mul(NANOS_PER_SECOND as i64)
}

/// A RTC device following the PL031 specification..
pub struct Rtc {
    previous_now: Instant,
    tick_offset: i64,
    // This is used for implementing the RTC alarm. However, in Firecracker we do not need it.
    match_value: u32,
    // Writes to this register load an update value into the RTC.
    load: u32,
    imsc: u32,
    ris: u32,
    // Retained for a future alarm implementation and test observability.
    #[allow(dead_code)]
    interrupt: Arc<dyn InterruptSourceGroup>,
}

impl Rtc {
    /// Constructs an AMBA PL031 RTC device.
    pub fn new(interrupt: Arc<dyn InterruptSourceGroup>) -> Self {
        Self {
            // This is used only for duration measuring purposes.
            previous_now: Instant::now(),
            tick_offset: get_time(ClockType::Real) as i64,
            match_value: 0,
            load: 0,
            imsc: 0,
            ris: 0,
            interrupt,
        }
    }

    fn get_time(&self) -> u32 {
        let ts = (self.tick_offset as i128)
            + (Instant::now().duration_since(self.previous_now).as_nanos() as i128);
        (ts / NANOS_PER_SECOND as i128) as u32
    }

    fn handle_write(&mut self, offset: u64, val: u32) -> Result<()> {
        match offset {
            RTCMR => {
                // The MR register is used for implementing the RTC alarm. A real time clock alarm is
                // a feature that can be used to allow a computer to 'wake up' after shut down to execute
                // tasks every day or on a certain day. It can sometimes be found in the 'Power Management'
                // section of a motherboard's BIOS setup. This is functionality that extends beyond
                // Firecracker intended use. However, we increment a metric just in case.
                self.match_value = val;
            }
            RTCLR => {
                self.load = val;
                self.previous_now = Instant::now();
                // If the unwrap fails, then the internal value of the clock has been corrupted and
                // we want to terminate the execution of the process.
                self.tick_offset = seconds_to_nanoseconds(i64::from(val)).unwrap();
            }
            RTCIMSC => {
                // The alarm is not implemented, so changing the mask cannot
                // assert an interrupt.
                self.imsc = val & 1;
            }
            RTCICR => {
                // PL031 clears the raw interrupt on any write. The alarm is not
                // implemented, so this must not raise a spurious IRQ.
                self.ris = 0;
            }
            RTCCR => (), // ignore attempts to turn off the timer.
            o => {
                return Err(Error::BadWriteOffset(o));
            }
        }
        Ok(())
    }
}

impl BusDevice for Rtc {
    fn read(&mut self, _base: u64, offset: u64, data: &mut [u8]) {
        let mut read_ok = true;

        let v = if (AMBA_ID_LOW..AMBA_ID_HIGH).contains(&offset) {
            let index = ((offset - AMBA_ID_LOW) >> 2) as usize;
            u32::from(PL031_ID[index])
        } else {
            match offset {
                RTCDR => self.get_time(),
                RTCMR => {
                    // Even though we are not implementing RTC alarm we return the last value
                    self.match_value
                }
                RTCLR => self.load,
                RTCCR => 1, // RTC is always enabled.
                RTCIMSC => self.imsc,
                RTCRIS => self.ris,
                RTCMIS => self.ris & self.imsc,
                _ => {
                    read_ok = false;
                    0
                }
            }
        };
        if read_ok && data.len() == 4 {
            write_le_u32(data, v);
        } else {
            warn!(
                "Invalid RTC PL031 read: offset {}, data length {}",
                offset,
                data.len()
            );
        }
    }

    fn write(&mut self, _base: u64, offset: u64, data: &[u8]) -> Option<Arc<Barrier>> {
        if data.len() == 4 {
            let v = read_le_u32(data);
            if let Err(e) = self.handle_write(offset, v) {
                warn!("Failed to write to RTC PL031 device: {}", e);
            }
        } else {
            warn!(
                "Invalid RTC PL031 write: offset {}, data length {}",
                offset,
                data.len()
            );
        }

        None
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        read_be_u16, read_be_u32, read_le_i32, read_le_u16, read_le_u32, read_le_u64, write_be_u16,
        write_be_u32, write_le_i32, write_le_u16, write_le_u32, write_le_u64,
    };
    use std::sync::Arc;
    use vm_device::interrupt::{InterruptIndex, InterruptSourceConfig};
    use vmm_sys_util::eventfd::EventFd;

    const LEGACY_RTC_MAPPED_IO_START: u64 = 0x0901_0000;

    #[test]
    fn test_get_time() {
        for _ in 0..1000 {
            assert!(get_time(ClockType::Monotonic) <= get_time(ClockType::Monotonic));
        }

        for _ in 0..1000 {
            assert!(get_time(ClockType::ProcessCpu) <= get_time(ClockType::ProcessCpu));
        }

        for _ in 0..1000 {
            assert!(get_time(ClockType::ThreadCpu) <= get_time(ClockType::ThreadCpu));
        }

        assert_ne!(get_time(ClockType::Real), 0);
    }

    #[test]
    fn test_local_time_display() {
        let local_time = LocalTime {
            sec: 30,
            min: 15,
            hour: 10,
            mday: 4,
            mon: 6,
            year: 119,
            nsec: 123_456_789,
        };
        assert_eq!(
            String::from("2019-07-04T10:15:30.123456789"),
            local_time.to_string()
        );

        let local_time = LocalTime {
            sec: 5,
            min: 5,
            hour: 5,
            mday: 23,
            mon: 7,
            year: 44,
            nsec: 123,
        };
        assert_eq!(
            String::from("1944-08-23T05:05:05.000000123"),
            local_time.to_string()
        );

        let local_time = LocalTime::now();
        assert!(local_time.mon >= 0 && local_time.mon <= 11);
    }

    #[test]
    fn test_seconds_to_nanoseconds() {
        assert_eq!(
            seconds_to_nanoseconds(100).unwrap() as u64,
            100 * NANOS_PER_SECOND
        );

        assert!(seconds_to_nanoseconds(9_223_372_037).is_none());
    }

    struct TestInterrupt {
        event_fd: EventFd,
    }

    impl InterruptSourceGroup for TestInterrupt {
        fn trigger(&self, _index: InterruptIndex) -> result::Result<(), std::io::Error> {
            self.event_fd.write(1)
        }

        fn update(
            &self,
            _index: InterruptIndex,
            _config: InterruptSourceConfig,
            _masked: bool,
        ) -> result::Result<(), std::io::Error> {
            Ok(())
        }

        fn notifier(&self, _index: InterruptIndex) -> Option<EventFd> {
            Some(self.event_fd.try_clone().unwrap())
        }
    }

    impl TestInterrupt {
        fn new(event_fd: EventFd) -> Self {
            TestInterrupt { event_fd }
        }
    }

    #[test]
    fn test_rtc_read_write_and_event() {
        let intr_evt = EventFd::new(libc::EFD_NONBLOCK).unwrap();

        let mut rtc = Rtc::new(Arc::new(TestInterrupt::new(intr_evt.try_clone().unwrap())));
        let mut data = [0; 4];

        // Read and write to the MR register.
        write_le_u32(&mut data, 123);
        rtc.write(LEGACY_RTC_MAPPED_IO_START, RTCMR, &data);
        rtc.read(LEGACY_RTC_MAPPED_IO_START, RTCMR, &mut data);
        let v = read_le_u32(&data);
        assert_eq!(v, 123);

        // Read and write to the LR register.
        let v = get_time(ClockType::Real);
        write_le_u32(&mut data, (v / NANOS_PER_SECOND) as u32);
        let previous_now_before = rtc.previous_now;
        rtc.write(LEGACY_RTC_MAPPED_IO_START, RTCLR, &data);

        assert!(rtc.previous_now > previous_now_before);

        rtc.read(LEGACY_RTC_MAPPED_IO_START, RTCLR, &mut data);
        let v_read = read_le_u32(&data);
        assert_eq!((v / NANOS_PER_SECOND) as u32, v_read);

        // Read and write to IMSC register. The alarm is not implemented,
        // so changing the mask must not assert an interrupt.
        let non_zero = 1;
        write_le_u32(&mut data, non_zero);
        rtc.write(LEGACY_RTC_MAPPED_IO_START, RTCIMSC, &data);
        assert_eq!(
            rtc.interrupt
                .notifier(0)
                .unwrap()
                .read()
                .unwrap_err()
                .kind(),
            std::io::ErrorKind::WouldBlock
        );
        rtc.read(LEGACY_RTC_MAPPED_IO_START, RTCIMSC, &mut data);
        let v = read_le_u32(&data);
        assert_eq!(non_zero & 1, v);

        // Now test with 0.
        write_le_u32(&mut data, 0);
        rtc.write(LEGACY_RTC_MAPPED_IO_START, RTCIMSC, &data);
        rtc.read(LEGACY_RTC_MAPPED_IO_START, RTCIMSC, &mut data);
        let v = read_le_u32(&data);
        assert_eq!(0, v);

        // Read and write to the ICR register. Clearing an unasserted alarm
        // must not generate a guest interrupt.
        write_le_u32(&mut data, 1);
        rtc.write(LEGACY_RTC_MAPPED_IO_START, RTCICR, &data);
        assert_eq!(
            rtc.interrupt
                .notifier(0)
                .unwrap()
                .read()
                .unwrap_err()
                .kind(),
            std::io::ErrorKind::WouldBlock
        );
        let v_before = read_le_u32(&data);

        rtc.read(LEGACY_RTC_MAPPED_IO_START, RTCICR, &mut data);
        let v = read_le_u32(&data);
        // ICR is a  write only register. Data received should stay equal to data sent.
        assert_eq!(v, v_before);

        // Attempts to turn off the RTC should not go through.
        write_le_u32(&mut data, 0);
        rtc.write(LEGACY_RTC_MAPPED_IO_START, RTCCR, &data);
        rtc.read(LEGACY_RTC_MAPPED_IO_START, RTCCR, &mut data);
        let v = read_le_u32(&data);
        assert_eq!(v, 1);

        // Attempts to write beyond the writable space. Using here the space used to read
        // the CID and PID from.
        write_le_u32(&mut data, 0);
        rtc.write(LEGACY_RTC_MAPPED_IO_START, AMBA_ID_LOW, &data);
        // However, reading from the AMBA_ID_LOW should succeed upon read.

        let mut data = [0; 4];
        rtc.read(LEGACY_RTC_MAPPED_IO_START, AMBA_ID_LOW, &mut data);
        let index = AMBA_ID_LOW + 3;
        assert_eq!(data[0], PL031_ID[((index - AMBA_ID_LOW) >> 2) as usize]);
    }

    #[test]
    fn test_rtc_rejects_non_word_access() {
        let intr_evt = EventFd::new(libc::EFD_NONBLOCK).unwrap();
        let mut rtc = Rtc::new(Arc::new(TestInterrupt::new(intr_evt)));
        let original_load = rtc.load;

        for len in [1, 2, 3, 5] {
            let mut data = vec![0xff; len];
            rtc.write(LEGACY_RTC_MAPPED_IO_START, RTCLR, &data);
            assert_eq!(rtc.load, original_load);

            rtc.read(LEGACY_RTC_MAPPED_IO_START, RTCLR, &mut data);
            assert_eq!(data, vec![0xff; len]);
        }
    }

    macro_rules! byte_order_test_read_write {
        ($test_name: ident, $write_fn_name: ident, $read_fn_name: ident, $is_be: expr, $data_type: ty) => {
            #[test]
            fn $test_name() {
                #[allow(overflowing_literals)]
                let test_cases = [
                    (
                        0x0123_4567_89AB_CDEF as u64,
                        [0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef],
                    ),
                    (
                        0x0000_0000_0000_0000 as u64,
                        [0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00],
                    ),
                    (
                        0x1923_2345_ABF3_CCD4 as u64,
                        [0x19, 0x23, 0x23, 0x45, 0xAB, 0xF3, 0xCC, 0xD4],
                    ),
                    (
                        0x0FF0_0FF0_0FF0_0FF0 as u64,
                        [0x0F, 0xF0, 0x0F, 0xF0, 0x0F, 0xF0, 0x0F, 0xF0],
                    ),
                    (
                        0xFFFF_FFFF_FFFF_FFFF as u64,
                        [0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF],
                    ),
                    (
                        0x89AB_12D4_C2D2_09BB as u64,
                        [0x89, 0xAB, 0x12, 0xD4, 0xC2, 0xD2, 0x09, 0xBB],
                    ),
                ];

                let type_size = std::mem::size_of::<$data_type>();
                for (test_val, v_arr) in &test_cases {
                    let v = *test_val as $data_type;
                    let cmp_iter: Box<dyn Iterator<Item = _>> = if $is_be {
                        Box::new(v_arr[(8 - type_size)..].iter())
                    } else {
                        Box::new(v_arr.iter().rev())
                    };
                    // test write
                    let mut write_arr = vec![Default::default(); type_size];
                    $write_fn_name(&mut write_arr, v);
                    for (cmp, cur) in cmp_iter.zip(write_arr.iter()) {
                        assert_eq!(*cmp, *cur as u8)
                    }
                    // test read
                    let read_val = $read_fn_name(&write_arr);
                    assert_eq!(v, read_val);
                }
            }
        };
    }

    byte_order_test_read_write!(test_le_u16, write_le_u16, read_le_u16, false, u16);
    byte_order_test_read_write!(test_le_u32, write_le_u32, read_le_u32, false, u32);
    byte_order_test_read_write!(test_le_u64, write_le_u64, read_le_u64, false, u64);
    byte_order_test_read_write!(test_le_i32, write_le_i32, read_le_i32, false, i32);
    byte_order_test_read_write!(test_be_u16, write_be_u16, read_be_u16, true, u16);
    byte_order_test_read_write!(test_be_u32, write_be_u32, read_be_u32, true, u32);
}
