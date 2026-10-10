// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use std::collections::HashSet;
use std::io::Write;
use std::time::Duration;

use crate::{error::DomainError, init::VolumeMount};

#[derive(Default)]
pub(crate) struct InitEffects {
    last_ca: String,
    ca_cleanup: Option<tokio::task::JoinHandle<()>>,
    mounted: HashSet<String>,
    mounts_complete: bool,
    pub(crate) hyperloop: Option<tokio::sync::mpsc::Sender<String>>,
}

impl InitEffects {
    pub(crate) async fn install_ca(&mut self, pem: String) -> Result<(), DomainError> {
        if pem.is_empty() {
            return Ok(());
        }
        // Serialize certificate I/O with the previous cleanup, keeping only one
        // background job. Other init fields and health do not wait for it.
        if let Some(cleanup) = self.ca_cleanup.take() {
            cleanup.await.map_err(|_| DomainError::Internal)?;
        }
        let normalized = pem.trim_end_matches('\n').to_owned() + "\n";
        if self.last_ca == normalized {
            return Ok(());
        }
        let cert = normalized.clone();
        tokio::task::spawn_blocking(move || {
            let mut file = std::fs::OpenOptions::new()
                .append(true)
                .open(CA_BUNDLE)
                .map_err(|e| ca_error("open CA bundle", "open", e))?;
            file.write_all(cert.as_bytes())
                .map_err(|e| ca_error("append CA cert", "write", e))
        })
        .await
        .map_err(|_| DomainError::Internal)??;
        let previous = std::mem::replace(&mut self.last_ca, normalized.clone());
        self.ca_cleanup = Some(tokio::task::spawn_blocking(move || {
            let persist = || -> std::io::Result<()> {
                std::fs::create_dir_all("/usr/local/share/ca-certificates")?;
                std::fs::write("/usr/local/share/ca-certificates/e2b-ca.crt", &normalized)
            };
            if let Err(error) = persist() {
                tracing::warn!(errno = error.raw_os_error(), "CA persistence failed");
            }
            // A failed persistence attempt must not prevent old trust removal.
            if !previous.is_empty() {
                if let Err(error) = remove_ca(&previous) {
                    tracing::warn!(errno = error.raw_os_error(), "CA rotation failed");
                }
            }
        }));
        Ok(())
    }

    pub(crate) async fn mount_nfs(&mut self, mounts: Vec<VolumeMount>) {
        if self.mounts_complete {
            return;
        }
        use futures::StreamExt;
        let deadline = tokio::time::Instant::now() + Duration::from_secs(5);
        let mut jobs = futures::stream::iter(mounts.into_iter().filter(|mount| !self.mounted.contains(&mount.path)).map(|mount| async move {
            let result = tokio::time::timeout_at(deadline, async {
                for (program, args) in [
                    ("mkdir", vec!["-p", mount.path.as_str()]),
                    ("mount", vec!["-v", "-t", "nfs", "-o", "fg,hard,sync,rsize=1048576,wsize=1048576,mountproto=tcp,mountport=2049,proto=tcp,port=2049,nfsvers=3,noacl", mount.nfs_target.as_str(), mount.path.as_str()]),
                ] {
                    let status = tokio::process::Command::new(program).args(args).kill_on_drop(true)
                        .stdout(std::process::Stdio::null()).stderr(std::process::Stdio::null()).status().await?;
                    if !status.success() { return Err(std::io::Error::other("mount command failed")); }
                }
                Ok(())
            }).await;
            (mount.path, matches!(result, Ok(Ok(()))))
        })).buffer_unordered(8);
        let mut completed = Vec::new();
        let mut success = true;
        while let Some((path, mounted)) = jobs.next().await {
            if mounted {
                completed.push(path);
            } else {
                success = false;
                tracing::warn!("NFS mount failed");
            }
        }
        drop(jobs);
        self.mounted.extend(completed);
        self.mounts_complete = success;
    }
}

const CA_BUNDLE: &str = "/etc/ssl/certs/ca-certificates.crt";

fn remove_ca(previous: &str) -> std::io::Result<()> {
    use std::os::unix::fs::PermissionsExt;
    let content = std::fs::read_to_string(CA_BUNDLE)?.replace(previous, "");
    let mut temp = tempfile::NamedTempFile::new_in("/etc/ssl/certs")?;
    temp.as_file()
        .set_permissions(std::fs::Permissions::from_mode(0o644))?;
    temp.write_all(content.as_bytes())?;
    temp.persist(CA_BUNDLE).map_err(|e| e.error)?;
    Ok(())
}

fn ca_error(operation: &str, syscall: &str, error: std::io::Error) -> DomainError {
    let detail = error.to_string().to_lowercase();
    let detail = detail.split(" (os error ").next().unwrap_or(&detail);
    DomainError::InvalidArgument(format!("failed to install CA bundle: {operation}: {syscall} /etc/ssl/certs/ca-certificates.crt: {detail}"))
}

pub(crate) async fn hyperloop(address: &str) -> bool {
    let Ok(ip) = address.parse::<std::net::IpAddr>() else {
        return false;
    };
    let Ok(hosts) = tokio::fs::read_to_string("/etc/hosts").await else {
        return false;
    };
    if hosts.lines().any(|line| {
        let mut parts = line.split('#').next().unwrap_or("").split_whitespace();
        parts.next().is_some_and(|first| first == address)
            && parts.any(|name| name == "events.e2b.local")
    }) {
        return true;
    }
    let mut updated = String::new();
    for line in hosts.lines() {
        let (entry, comment) = line.split_once('#').unwrap_or((line, ""));
        let mut parts = entry.split_whitespace();
        let Some(first) = parts.next() else {
            updated.push_str(line);
            updated.push('\n');
            continue;
        };
        let same_family = first
            .parse::<std::net::IpAddr>()
            .is_ok_and(|old| old.is_ipv4() == ip.is_ipv4());
        let names: Vec<_> = parts
            .filter(|name| !same_family || *name != "events.e2b.local")
            .collect();
        if !names.is_empty() {
            updated.push_str(first);
            updated.push('\t');
            updated.push_str(&names.join(" "));
            if !comment.is_empty() {
                updated.push_str(" #");
                updated.push_str(comment);
            }
            updated.push('\n');
        }
    }
    updated.push_str(&format!("{address}\tevents.e2b.local\n"));
    tokio::fs::write("/etc/hosts", updated).await.is_ok()
}

pub(crate) async fn mmds_hash() -> Option<String> {
    super::metadata::current_metadata()
        .await
        .map(|metadata| metadata.access_token_hash.to_string())
        .filter(|hash| !hash.is_empty())
}
