// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use crate::error::DomainError;
use crate::runtime::{RuntimeState, RuntimeStateStore, UserDatabase};
use std::collections::BTreeMap;
use std::fmt;
use std::sync::Arc;

use serde::de::{self, Deserializer, IgnoredAny, MapAccess, Visitor};
use serde::Deserialize;

#[derive(Default)]
pub struct InitRequest {
    pub env_vars: Option<BTreeMap<String, String>>,
    pub access_token: Option<zeroize::Zeroizing<String>>,
    pub ca_bundle: Option<String>,
    pub default_user: Option<String>,
    pub default_workdir: Option<String>,
    pub hyperloop_ip: Option<String>,
    pub timestamp: Option<String>,
    pub volume_mounts: Option<Vec<VolumeMount>>,
}

#[derive(Debug, Default)]
pub struct VolumeMount {
    pub nfs_target: String,
    pub path: String,
}

#[derive(Debug, PartialEq, Eq)]
pub enum ParseError {
    TooDeep,
    Malformed,
}

impl<'de> Deserialize<'de> for InitRequest {
    fn deserialize<D>(deserializer: D) -> Result<Self, D::Error>
    where
        D: Deserializer<'de>,
    {
        deserializer.deserialize_map(InitRequestVisitor)
    }
}

struct InitRequestVisitor;

impl<'de> Visitor<'de> for InitRequestVisitor {
    type Value = InitRequest;

    fn expecting(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("an init request object")
    }

    fn visit_map<M>(self, mut map: M) -> Result<Self::Value, M::Error>
    where
        M: MapAccess<'de>,
    {
        let mut request = InitRequest::default();

        while let Some(key) = map.next_key::<String>()? {
            match key.to_ascii_lowercase().as_str() {
                "envvars" => match map.next_value::<Option<BTreeMap<String, Option<String>>>>()? {
                    Some(values) => request
                        .env_vars
                        .get_or_insert_with(BTreeMap::new)
                        .extend(values.into_iter().map(|(k, v)| (k, v.unwrap_or_default()))),
                    None => request.env_vars = None,
                },
                "accesstoken" => {
                    request.access_token =
                        match map.next_value::<Option<&serde_json::value::RawValue>>()? {
                            None => None,
                            Some(raw) => {
                                let raw = raw.get();
                                if raw.len() <= 2
                                    || !raw.starts_with('"')
                                    || !raw.ends_with('"')
                                    || raw.contains('\\')
                                {
                                    return Err(de::Error::custom("invalid secure token"));
                                }
                                Some(zeroize::Zeroizing::new(raw[1..raw.len() - 1].to_owned()))
                            }
                        };
                }
                "cabundle" => request.ca_bundle = map.next_value()?,
                "defaultuser" => request.default_user = map.next_value()?,
                "defaultworkdir" => request.default_workdir = map.next_value()?,
                "hyperloopip" => request.hyperloop_ip = map.next_value()?,
                "timestamp" => request.timestamp = map.next_value()?,
                "volumemounts" => request.volume_mounts = map.next_value()?,
                _ => {
                    map.next_value::<IgnoredAny>()?;
                }
            }
        }
        Ok(request)
    }
}

pub fn parse(body: &[u8]) -> Result<InitRequest, ParseError> {
    if exceeds_depth(body, crate::transport::limits::INIT_JSON_DEPTH_LIMIT) {
        return Err(ParseError::TooDeep);
    }
    if body.trim_ascii() == b"null" {
        return Ok(InitRequest::default());
    }
    serde_json::from_slice(body).map_err(|_| ParseError::Malformed)
}

fn exceeds_depth(body: &[u8], limit: usize) -> bool {
    let mut depth = 0usize;
    let mut in_string = false;
    let mut escaped = false;
    for &byte in body {
        if in_string {
            if escaped {
                escaped = false;
            } else if byte == b'\\' {
                escaped = true;
            } else if byte == b'"' {
                in_string = false;
            }
            continue;
        }
        match byte {
            b'"' => in_string = true,
            b'{' | b'[' => {
                depth += 1;
                if depth > limit {
                    return true;
                }
            }
            b'}' | b']' => depth = depth.saturating_sub(1),
            _ => {}
        }
    }
    false
}

impl fmt::Debug for InitRequest {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("InitRequest").finish_non_exhaustive()
    }
}

impl<'de> Deserialize<'de> for VolumeMount {
    fn deserialize<D: Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        struct MountVisitor;
        impl<'de> Visitor<'de> for MountVisitor {
            type Value = VolumeMount;
            fn expecting(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
                f.write_str("a volume mount object")
            }
            fn visit_unit<E: de::Error>(self) -> Result<Self::Value, E> {
                Ok(VolumeMount::default())
            }
            fn visit_map<M: MapAccess<'de>>(self, mut map: M) -> Result<Self::Value, M::Error> {
                let mut mount = VolumeMount::default();
                while let Some(key) = map.next_key::<String>()? {
                    match key.to_ascii_lowercase().as_str() {
                        "path" => {
                            if let Some(value) = map.next_value::<Option<String>>()? {
                                mount.path = value;
                            }
                        }
                        "nfs_target" => {
                            if let Some(value) = map.next_value::<Option<String>>()? {
                                mount.nfs_target = value;
                            }
                        }
                        _ => {
                            map.next_value::<IgnoredAny>()?;
                        }
                    }
                }
                Ok(mount)
            }
        }
        deserializer.deserialize_any(MountVisitor)
    }
}

pub(crate) mod effects;
pub(crate) mod metadata;

pub fn write_sandbox_marker(is_sandbox: bool) -> std::io::Result<()> {
    use std::io::Write;
    use std::os::unix::fs::OpenOptionsExt;
    std::fs::create_dir_all("/run/e2b")?;
    let mut file = std::fs::OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(true)
        .mode(0o444)
        .open("/run/e2b/.E2B_SANDBOX")?;
    file.write_all(is_sandbox.to_string().as_bytes())
}

impl RuntimeStateStore {
    pub(crate) fn install_metadata_refresh(&self, refresh: tokio::sync::mpsc::Sender<()>) {
        if let Ok(mut slot) = self.metadata_refresh.lock() {
            *slot = Some(refresh);
        }
    }

    pub(crate) fn refresh_metadata(&self) {
        if let Ok(slot) = self.metadata_refresh.lock() {
            if let Some(refresh) = slot.as_ref() {
                let _ = refresh.try_send(());
            }
        }
    }

    pub(crate) async fn project_metadata(&self, metadata: &crate::init::metadata::Metadata) {
        {
            let mut slot = self.current.write().await;
            let mut updated = (**slot).clone();
            updated
                .environment
                .insert("E2B_SANDBOX_ID".into(), metadata.sandbox_id.clone());
            updated
                .environment
                .insert("E2B_TEMPLATE_ID".into(), metadata.template_id.clone());
            if updated.environment != slot.environment {
                updated.generation = updated.generation.saturating_add(1);
                *slot = Arc::new(updated);
            }
        }
        for (path, value) in [
            ("/run/e2b/.E2B_SANDBOX_ID", &metadata.sandbox_id),
            ("/run/e2b/.E2B_TEMPLATE_ID", &metadata.template_id),
        ] {
            if let Err(error) = tokio::fs::write(path, value).await {
                tracing::warn!(
                    errno = error.raw_os_error(),
                    path,
                    "metadata projection failed"
                );
            }
        }
    }

    pub async fn apply(
        &self,
        request: InitRequest,
        _users: &UserDatabase,
    ) -> Result<(), DomainError> {
        let effects = self.updates.clone().lock_owned().await;
        self.apply_locked(request, effects).await
    }

    pub(crate) async fn apply_locked(
        &self,
        request: InitRequest,
        mut effects: tokio::sync::OwnedMutexGuard<crate::init::effects::InitEffects>,
    ) -> Result<(), DomainError> {
        let timestamp = request
            .timestamp
            .as_deref()
            .map(parse_timestamp)
            .transpose()?;

        let mut timestamp_changed = false;
        // AtomicMax in the oracle accepts equal instants and advances before SetData.
        if let Some(timestamp) = timestamp.as_ref() {
            let mut slot = self.current.write().await;
            let current = slot.clone();
            let next = timestamp.unix_nanos();
            let previous = current
                .last_successful_init
                .as_ref()
                .map(InitTimestamp::unix_nanos)
                .unwrap_or(0);
            if next < previous {
                return Ok(());
            }
            timestamp_changed = next != previous;
            let mut advanced = (*current).clone();
            advanced.last_successful_init = Some(timestamp.clone());
            *slot = Arc::new(advanced);
        }
        {
            use sha2::{Digest, Sha512};
            use subtle::ConstantTimeEq;
            let token = self.access_token.read().await;
            let matches = token
                .as_ref()
                .zip(request.access_token.as_ref())
                .is_some_and(|(old, next)| bool::from(old.as_bytes().ct_eq(next.as_bytes())));
            if !matches {
                let hash = if self.is_sandbox {
                    crate::init::effects::mmds_hash().await
                } else {
                    None
                };
                let requested = request
                    .access_token
                    .as_ref()
                    .map(|v| v.as_bytes())
                    .unwrap_or_default();
                let matches_mmds = hash.as_ref().is_some_and(|hash| {
                    bool::from(
                        hash.as_bytes()
                            .ct_eq(format!("{:x}", Sha512::digest(requested)).as_bytes()),
                    )
                });
                if !matches_mmds && (token.is_some() || hash.is_some()) {
                    return Err(DomainError::Unauthenticated);
                }
            }
        }
        if let Some(timestamp) = timestamp.as_ref() {
            timestamp.set_system_time();
        }
        *self.access_token.write().await = request.access_token;
        let mut slot = self.current.write().await;
        let current = slot.clone();
        let mut environment = current.environment.clone();
        if let Some(env_vars) = request.env_vars {
            for (key, value) in env_vars {
                environment.insert(key, value);
            }
        }

        let mut default_user = current.default_user.clone();
        if let Some(user) = request.default_user.filter(|user| !user.is_empty()) {
            default_user = user;
        }

        let mut default_workdir = current.default_workdir.clone();
        if let Some(workdir) = request
            .default_workdir
            .filter(|workdir| !workdir.is_empty())
        {
            default_workdir = Some(workdir);
        }

        let last_successful_init = timestamp.or_else(|| current.last_successful_init.clone());
        if !timestamp_changed
            && environment == current.environment
            && default_user == current.default_user
            && default_workdir == current.default_workdir
            && last_successful_init == current.last_successful_init
            && request.ca_bundle.is_none()
            && request.volume_mounts.is_none()
            && request.hyperloop_ip.is_none()
        {
            return Ok(());
        }
        let generation = current
            .generation
            .checked_add(1)
            .ok_or(DomainError::Internal)?;
        *slot = Arc::new(RuntimeState {
            environment,
            default_user,
            default_workdir,
            last_successful_init,
            generation,
        });
        drop(slot);
        if let Some(address) = request.hyperloop_ip {
            if effects.hyperloop.is_none() {
                let (sender, mut receiver) = tokio::sync::mpsc::channel::<String>(1);
                let current = self.current.clone();
                tokio::spawn(async move {
                    while let Some(address) = receiver.recv().await {
                        if crate::init::effects::hyperloop(&address).await {
                            let mut slot = current.write().await;
                            let mut updated = (**slot).clone();
                            updated
                                .environment
                                .insert("E2B_EVENTS_ADDRESS".into(), format!("http://{address}"));
                            *slot = Arc::new(updated);
                        }
                    }
                });
                effects.hyperloop = Some(sender);
            }
            // A single pending update applies backpressure instead of spawning
            // unbounded jobs or losing accepted hosts changes.
            effects
                .hyperloop
                .as_ref()
                .expect("hyperloop worker")
                .send(address)
                .await
                .map_err(|_| DomainError::Internal)?;
        }
        if let Some(pem) = request.ca_bundle {
            effects.install_ca(pem).await?;
        }
        if let Some(mounts) = request.volume_mounts {
            effects.mount_nfs(mounts).await;
        }
        Ok(())
    }
}

#[derive(Debug, Clone)]
pub struct InitTimestamp {
    absolute_second: i64,
    nanosecond: u32,
    canonical: String,
}

impl fmt::Display for InitTimestamp {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        self.canonical.fmt(formatter)
    }
}

impl PartialEq for InitTimestamp {
    fn eq(&self, other: &Self) -> bool {
        (self.absolute_second, self.nanosecond) == (other.absolute_second, other.nanosecond)
    }
}

impl Eq for InitTimestamp {}

impl PartialOrd for InitTimestamp {
    fn partial_cmp(&self, other: &Self) -> Option<std::cmp::Ordering> {
        Some(self.cmp(other))
    }
}

impl Ord for InitTimestamp {
    fn cmp(&self, other: &Self) -> std::cmp::Ordering {
        (self.absolute_second, self.nanosecond).cmp(&(other.absolute_second, other.nanosecond))
    }
}

fn parse_timestamp(raw: &str) -> Result<InitTimestamp, DomainError> {
    // Go's RFC3339 parser accepts a one-digit hour and a comma fraction.
    let mut normalized = raw.replace(',', ".");
    if normalized.as_bytes().get(12) == Some(&b':') {
        normalized.insert(11, '0');
    }
    let raw = normalized.as_str();
    let bytes = raw.as_bytes();
    if bytes.len() < 20
        || ![0, 1, 2, 3, 5, 6, 8, 9, 11, 12, 14, 15, 17, 18]
            .into_iter()
            .all(|index| bytes.get(index).is_some_and(u8::is_ascii_digit))
        || bytes.get(4) != Some(&b'-')
        || bytes.get(7) != Some(&b'-')
        || !matches!(bytes.get(10), Some(b'T'))
        || bytes.get(13) != Some(&b':')
        || bytes.get(16) != Some(&b':')
        || &bytes[17..19] == b"60"
    {
        return Err(invalid_timestamp());
    }

    let (fraction, offset) = match bytes.get(19) {
        Some(b'.') => {
            let end = bytes[20..]
                .iter()
                .position(|byte| matches!(byte, b'Z' | b'+' | b'-'))
                .map(|index| index + 20)
                .ok_or_else(invalid_timestamp)?;
            if end == 20 || !bytes[20..end].iter().all(u8::is_ascii_digit) {
                return Err(invalid_timestamp());
            }
            (Some(&raw[20..end]), &raw[end..])
        }
        Some(b'Z' | b'+' | b'-') => (None, &raw[19..]),
        _ => return Err(invalid_timestamp()),
    };
    if !valid_offset(offset) {
        return Err(invalid_timestamp());
    }

    let mut civil_text = raw[..19].to_owned();
    civil_text.replace_range(10..11, "T");
    if let Some(fraction) = fraction {
        civil_text.push('.');
        civil_text.extend(fraction.chars().take(9));
    }
    let civil: jiff::civil::DateTime = civil_text.parse().map_err(|_| invalid_timestamp())?;
    let offset_seconds = offset_seconds(offset);
    let days = days_from_civil(
        i64::from(civil.year()),
        i64::from(civil.month()),
        i64::from(civil.day()),
    );
    let absolute_second = days * 86_400
        + i64::from(civil.hour()) * 3_600
        + i64::from(civil.minute()) * 60
        + i64::from(civil.second())
        - offset_seconds;

    let mut canonical = raw[..19].to_owned();
    canonical.replace_range(10..11, "T");
    if let Some(fraction) = fraction {
        let truncated: String = fraction.chars().take(9).collect();
        let fraction = truncated.trim_end_matches('0');
        if !fraction.is_empty() {
            canonical.push('.');
            canonical.push_str(fraction);
        }
    }
    if offset.eq_ignore_ascii_case("z") {
        canonical.push('Z');
    } else {
        canonical.push_str(offset);
    }
    Ok(InitTimestamp {
        absolute_second,
        nanosecond: u32::try_from(civil.subsec_nanosecond()).map_err(|_| invalid_timestamp())?,
        canonical,
    })
}

fn valid_offset(offset: &str) -> bool {
    if offset.eq_ignore_ascii_case("z") {
        return true;
    }
    let bytes = offset.as_bytes();
    bytes.len() == 6
        && matches!(bytes[0], b'+' | b'-')
        && bytes[1..3].iter().all(u8::is_ascii_digit)
        && bytes[3] == b':'
        && bytes[4..6].iter().all(u8::is_ascii_digit)
        && (bytes[1] - b'0') * 10 + bytes[2] - b'0' <= 24
        && (bytes[4] - b'0') * 10 + bytes[5] - b'0' <= 60
}

fn offset_seconds(offset: &str) -> i64 {
    if offset.eq_ignore_ascii_case("z") {
        return 0;
    }
    let bytes = offset.as_bytes();
    let seconds = i64::from((bytes[1] - b'0') * 10 + bytes[2] - b'0') * 3_600
        + i64::from((bytes[4] - b'0') * 10 + bytes[5] - b'0') * 60;
    if bytes[0] == b'-' {
        -seconds
    } else {
        seconds
    }
}

fn days_from_civil(year: i64, month: i64, day: i64) -> i64 {
    let year = year - i64::from(month <= 2);
    let era = if year >= 0 { year } else { year - 399 } / 400;
    let year_of_era = year - era * 400;
    let shifted_month = month + if month > 2 { -3 } else { 9 };
    let day_of_year = (153 * shifted_month + 2) / 5 + day - 1;
    era * 146_097 + year_of_era * 365 + year_of_era / 4 - year_of_era / 100 + day_of_year - 719_468
}

fn invalid_timestamp() -> DomainError {
    DomainError::InvalidArgument("invalid timestamp".to_owned())
}

impl InitTimestamp {
    fn unix_nanos(&self) -> i64 {
        self.absolute_second
            .wrapping_mul(1_000_000_000)
            .wrapping_add(i64::from(self.nanosecond))
    }

    fn set_system_time(&self) {
        let now = jiff::Timestamp::now().as_nanosecond();
        let target = i128::from(self.unix_nanos());
        if now < target - 50_000_000 || now > target + 5_000_000_000 {
            let nanos = self.unix_nanos();
            let time = libc::timespec {
                tv_sec: nanos.div_euclid(1_000_000_000),
                tv_nsec: nanos.rem_euclid(1_000_000_000),
            };
            // The daemon runs in the guest; lack of SYS_TIME is nonfatal upstream.
            if unsafe { libc::clock_settime(libc::CLOCK_REALTIME, &time) } != 0 {
                tracing::warn!(
                    errno = std::io::Error::last_os_error().raw_os_error(),
                    "setting guest time failed"
                );
            }
        }
    }
}
