// Copyright (c) 2019 Ant Financial
//
// SPDX-License-Identifier: Apache-2.0
//

use anyhow::{anyhow, Result};
use nix::mount::{self, MsFlags};
use slog::Logger;
use std::fs;
use std::io::ErrorKind;
use std::path::Path;

const KATA_GUEST_SANDBOX_DNS_FILE: &str = "/run/cube-containers/sandbox/resolv.conf";
const GUEST_DNS_FILE: &str = "/etc/resolv.conf";

// Network describes a sandbox network, including its dns
// related information.
#[derive(Debug, Default)]
pub struct Network {
    dns: Vec<String>,
}

impl Network {
    pub fn new() -> Network {
        Network { dns: Vec::new() }
    }

    pub fn set_dns(&mut self, dns: String) {
        self.dns.push(dns);
    }
}

pub fn setup_guest_dns(logger: Logger, dns_list: Vec<String>) -> Result<()> {
    do_setup_guest_dns(
        logger,
        dns_list,
        KATA_GUEST_SANDBOX_DNS_FILE,
        GUEST_DNS_FILE,
    )
}

fn do_setup_guest_dns(logger: Logger, dns_list: Vec<String>, src: &str, dst: &str) -> Result<()> {
    let logger = logger.new(o!( "subsystem" => "network"));

    if dns_list.is_empty() {
        info!(
            logger,
            "Did not set sandbox DNS as DNS not received as part of request."
        );
        return Ok(());
    }

    if let Some(parent) = Path::new(src).parent() {
        fs::create_dir_all(parent)?;
    }

    match fs::metadata(dst) {
        Ok(attr) => {
            if attr.is_dir() {
                return Err(anyhow!("{} is a directory", GUEST_DNS_FILE));
            }
        }
        Err(err) if err.kind() == ErrorKind::NotFound => {
            warn!(
                logger,
                "{} missing in guest rootfs, creating it before DNS bind mount", dst
            );
            if let Some(parent) = Path::new(dst).parent() {
                fs::create_dir_all(parent)?;
            }
            fs::File::create(dst)?;
        }
        Err(err) => {
            return Err(anyhow!(err).context(format!("failed to stat {}", dst)));
        }
    }

    // write DNS to file
    let content = dns_list
        .iter()
        .map(|x| x.trim())
        .collect::<Vec<&str>>()
        .join("\n");
    fs::write(src, &content)?;

    // bind mount to /etc/resolv.conf
    mount::mount(Some(src), dst, Some("bind"), MsFlags::MS_BIND, None::<&str>)
        .map_err(|err| anyhow!(err).context("failed to setup guest DNS"))?;

    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::skip_if_not_root;
    use nix::mount;
    use std::fs::File;
    use std::io::Write;
    use tempfile::tempdir;

    #[test]
    fn test_setup_guest_dns() {
        skip_if_not_root!();

        let drain = slog::Discard;
        let logger = slog::Logger::root(drain, o!());

        // create temp for /run/kata-containers/sandbox/resolv.conf
        let src_dir = tempdir().expect("failed to create tmpdir");
        let tmp = src_dir.path().join("resolv.conf");
        let src_filename = tmp.to_str().expect("failed to get resolv file filename");

        // create temp for /etc/resolv.conf
        let dst_dir = tempdir().expect("failed to create tmpdir");
        let tmp = dst_dir.path().join("resolv.conf");
        let dst_filename = tmp.to_str().expect("failed to get resolv file filename");
        {
            let _file = File::create(dst_filename).unwrap();
        }

        // test DNS
        let dns = vec![
            "nameserver 1.2.3.4".to_string(),
            "nameserver 5.6.7.8".to_string(),
        ];

        // write to /run/kata-containers/sandbox/resolv.conf
        let mut src_file = File::create(src_filename)
            .unwrap_or_else(|_| panic!("failed to create file {:?}", src_filename));
        let content = dns.join("\n");
        src_file
            .write_all(content.as_bytes())
            .expect("failed to write file contents");

        // call do_setup_guest_dns
        let result = do_setup_guest_dns(logger, dns.clone(), src_filename, dst_filename);

        assert!(result.is_ok(), "result should be ok, but {:?}", result);

        // get content of /etc/resolv.conf
        let content = fs::read_to_string(dst_filename);
        assert!(content.is_ok());
        let content = content.unwrap();

        let expected_dns: Vec<&str> = content.split('\n').collect();

        // assert the data are the same as /run/kata-containers/sandbox/resolv.conf
        assert_eq!(dns, expected_dns);

        // umount /etc/resolv.conf
        let _ = mount::umount(dst_filename);
    }

    #[test]
    fn test_setup_guest_dns_creates_missing_destination_file() {
        skip_if_not_root!();

        let drain = slog::Discard;
        let logger = slog::Logger::root(drain, o!());

        let src_dir = tempdir().expect("failed to create tmpdir");
        let src_filename = src_dir
            .path()
            .join("sandbox")
            .join("resolv.conf")
            .to_str()
            .expect("failed to get resolv file filename")
            .to_string();

        let dst_dir = tempdir().expect("failed to create tmpdir");
        let dst_filename = dst_dir
            .path()
            .join("etc")
            .join("resolv.conf")
            .to_str()
            .expect("failed to get resolv file filename")
            .to_string();

        let dns = vec!["nameserver 1.1.1.1".to_string()];
        let result = do_setup_guest_dns(logger, dns.clone(), &src_filename, &dst_filename);

        assert!(result.is_ok(), "result should be ok, but {:?}", result);
        assert!(Path::new(&dst_filename).exists());

        let content = fs::read_to_string(&dst_filename);
        assert!(content.is_ok());
        assert_eq!(content.unwrap(), dns.join("\n"));

        let _ = mount::umount(dst_filename.as_str());
    }
}
