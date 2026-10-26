// Copyright © 2023 Tencent Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

use std::io::Write;
use std::sync::Mutex;

static mut JSON_FILE_NAME: Option<String> = None;
static mut JSON_FILE: Mutex<Option<Box<dyn std::io::Write + Send>>> = Mutex::new(None);

/// This function must only be called once from the main process before any threads
/// are created to avoid race conditions
pub fn set_log_file(filename: String) -> Result<(), std::io::Error> {
    // SAFETY: there is only one caller of this function, so JSON_FILE_NAME is written to only once
    assert!(unsafe { JSON_FILE_NAME.is_none() });
    // SAFETY: JSON_FILE_NAME is None. Nobody else can hold a reference to it.
    unsafe {
        JSON_FILE_NAME = Some(filename);
    };
    Ok(())
}

pub fn log_string(content: &str) {
    // SAFETY: JSON_FILE_NAME is read only here.
    if unsafe { JSON_FILE_NAME.is_none() } {
        return;
    }

    // SAFETY: JSON_FILE_NAME is valid and read only here.
    let mut output = unsafe { JSON_FILE.lock().unwrap() };
    if output.is_none() {
        // SAFETY: JSON_FILE_NAME is valid and read only here.
        if let Some(filename) = unsafe { JSON_FILE_NAME.as_ref() } {
            *output = match std::fs::File::options()
                .create(true)
                .append(true)
                .open(std::path::Path::new(&filename))
            {
                Ok(output) => Some(Box::new(output)),
                Err(_) => None,
            }
        }
    }

    if output.is_some() {
        let t = format!("{}\n", content);
        (*(output.as_mut().unwrap())).write_all(t.as_bytes()).ok();
    }
}

#[macro_export]
macro_rules! log_json {
    ($source:expr) => {
        $crate::log_string(&serde_json::to_string($source).unwrap())
    };
}
