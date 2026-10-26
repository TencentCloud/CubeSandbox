// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use anyhow::{anyhow, Ok, Result};
use containerd_shim_cube_rs::common::utils;
use lazy_static::lazy_static;
use regex::Regex;

lazy_static! {
    static ref VALID_CID_REGEX: Regex = Regex::new(r"^[a-zA-Z0-9][a-zA-Z0-9_.-]+$").unwrap();
}

/// Verifies if the provided container ID is valid.
/// Returns the ID if valid, otherwise returns an error.
/// Functional programming principles are used to avoid mutable state and side effects.
pub fn verify_container_id(id: &str) -> Result<String> {
    // Check if the ID is empty
    if id.is_empty() {
        return Err(anyhow!("container/sandbox ID cannot be empty"));
    }

    // Validate the ID against the regex pattern
    if !VALID_CID_REGEX.is_match(id) {
        return Err(anyhow!(
            "invalid container/sandbox ID (should match {})",
            VALID_CID_REGEX.as_str()
        ));
    }

    // Check if the default path exists
    let default_path = utils::Utils::vsock_path(id);
    if default_path.exists() {
        return Ok(id.to_string());
    }

    // Search for matching entries in the VM_PATH directory
    std::fs::read_dir(utils::VM_PATH)
        .map_err(|e| anyhow!("can not read directory {}: {}", utils::VM_PATH, e))?
        .filter_map(|entry| entry.ok())
        .find_map(|entry| {
            entry
                .path()
                .file_name()
                .and_then(|n| n.to_str())
                .filter(|n| n.starts_with(id))
                .map(|name| name.to_string())
        })
        .map(Ok)
        .unwrap_or_else(|| Ok(String::from(id)))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::{fs, path::Path};

    #[test]
    fn test_verify_container_id() {
        // Test empty ID
        assert!(verify_container_id("").is_err());

        // Test invalid ID
        assert!(verify_container_id("a").is_err());

        // Test valid ID with temporary directory
        let long_id = "7b20f3d17956f9a42b8c6e2792bcdbc37df9538b8cbdba330252db9c1b7acb5c";
        let short_id = &long_id[..9];
        let dir_name = Path::new(utils::VM_PATH).join(long_id);

        // Ensure the directory is created successfully
        let created = fs::create_dir_all(&dir_name);
        if created.is_ok() {
            // Test with the temporary directory
            assert!(verify_container_id(short_id).is_ok());

            // Clean up the temporary directory
            fs::remove_dir_all(&dir_name).expect("Failed to remove temporary directory");
            println!("Removed temporary directory: {:?}", dir_name);
        }
    }
}
