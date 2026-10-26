// Copyright © 2021 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

use std::{
    collections::VecDeque,
    io::Write,
    sync::{
        atomic::{AtomicBool, Ordering},
        Arc,
    },
};

const MAX_BUFFER_SIZE: usize = 1 << 20;

// Circular buffer implementation for serial output.
// Read from head; push to tail
pub struct SerialBuffer {
    buffer: VecDeque<u8>,
    out: Box<dyn Write + Send>,
    write_out: Arc<AtomicBool>,
}

impl SerialBuffer {
    pub fn new(out: Box<dyn Write + Send>, write_out: Arc<AtomicBool>) -> Self {
        Self {
            buffer: VecDeque::new(),
            out,
            write_out,
        }
    }

    fn fill_buffer(&mut self, buf: &[u8]) {
        if buf.len() >= MAX_BUFFER_SIZE {
            let offset = buf.len() - MAX_BUFFER_SIZE;
            self.buffer = VecDeque::from(buf[offset..].to_vec());
            return;
        }

        let num_allowed_bytes = MAX_BUFFER_SIZE - buf.len();
        if self.buffer.len() > num_allowed_bytes {
            let num_bytes_to_remove = self.buffer.len() - num_allowed_bytes;
            self.buffer.drain(..num_bytes_to_remove);
        }

        self.buffer.extend(buf);
    }
}

impl Write for SerialBuffer {
    fn write(&mut self, buf: &[u8]) -> Result<usize, std::io::Error> {
        // Simply fill the buffer if we're not allowed to write to the out
        // device.
        if !self.write_out.load(Ordering::Acquire) {
            self.fill_buffer(buf);
            return Ok(buf.len());
        }

        // In case we're allowed to write to the out device, we flush the
        // content of the buffer.
        self.flush()?;

        // If after flushing the buffer, it's still not empty, that means
        // only a subset of the bytes was written and we should fill the buffer
        // with what's coming from the serial.
        if !self.buffer.is_empty() {
            self.fill_buffer(buf);
            return Ok(buf.len());
        }

        // We reach this point if we're allowed to write to the out device
        // and we know there's nothing left in the buffer.
        let mut offset = 0;
        loop {
            match self.out.write(&buf[offset..]) {
                Ok(written_bytes) => {
                    if written_bytes < buf.len() - offset {
                        offset += written_bytes;
                        continue;
                    }
                }
                Err(e) => {
                    if !matches!(e.kind(), std::io::ErrorKind::WouldBlock) {
                        return Err(e);
                    }
                    self.fill_buffer(&buf[offset..]);
                }
            }
            break;
        }

        // Make sure we flush anything that might have been written to the
        // out device.
        self.out.flush()?;

        Ok(buf.len())
    }

    // This function flushes the content of the buffer to the out device if
    // it is allowed to, otherwise this is a no-op.
    fn flush(&mut self) -> Result<(), std::io::Error> {
        if !self.write_out.load(Ordering::Acquire) {
            return Ok(());
        }

        while let Some(byte) = self.buffer.pop_front() {
            if self.out.write_all(&[byte]).is_err() {
                self.buffer.push_front(byte);
                break;
            }
        }
        self.out.flush()
    }
}
