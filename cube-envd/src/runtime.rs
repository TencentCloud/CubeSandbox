// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};
use std::sync::Arc;

use base64::Engine;
use tokio::sync::{Mutex, RwLock};

use crate::error::DomainError;
use crate::init::InitTimestamp;

pub(crate) fn basic_username(headers: &http::HeaderMap) -> Option<Vec<u8>> {
    let value = headers.get(http::header::AUTHORIZATION)?.to_str().ok()?;
    if !value
        .get(..6)
        .is_some_and(|v| v.eq_ignore_ascii_case("Basic "))
    {
        return None;
    }
    let decoder = base64::engine::GeneralPurpose::new(
        &base64::alphabet::STANDARD,
        base64::engine::GeneralPurposeConfig::new().with_decode_allow_trailing_bits(true),
    );
    let bytes = decoder.decode(&value[6..]).ok()?;
    let colon = bytes.iter().position(|b| *b == b':')?;
    Some(bytes[..colon].to_vec())
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RuntimeState {
    pub(crate) environment: BTreeMap<String, String>,
    pub(crate) default_user: String,
    pub(crate) default_workdir: Option<String>,
    pub(crate) last_successful_init: Option<InitTimestamp>,
    pub(crate) generation: u64,
}

impl RuntimeState {
    pub fn environment(&self) -> &BTreeMap<String, String> {
        &self.environment
    }

    pub fn default_user(&self) -> &str {
        &self.default_user
    }

    pub fn default_workdir(&self) -> Option<&str> {
        self.default_workdir.as_deref()
    }

    pub fn last_successful_init(&self) -> Option<&InitTimestamp> {
        self.last_successful_init.as_ref()
    }

    pub fn generation(&self) -> u64 {
        self.generation
    }
}

pub struct RuntimeStateStore {
    pub(crate) current: Arc<RwLock<Arc<RuntimeState>>>,
    pub(crate) updates: Arc<Mutex<crate::init::effects::InitEffects>>,
    pub(crate) is_sandbox: bool,
    pub(crate) access_token: RwLock<Option<zeroize::Zeroizing<String>>>,
}

impl RuntimeStateStore {
    pub fn new() -> Self {
        Self::with_sandbox_mode(false)
    }

    pub(crate) fn with_sandbox_mode(is_sandbox: bool) -> Self {
        let environment = BTreeMap::from([("E2B_SANDBOX".to_owned(), is_sandbox.to_string())]);
        Self {
            current: Arc::new(RwLock::new(Arc::new(RuntimeState {
                environment,
                default_user: "root".to_owned(),
                default_workdir: None,
                last_successful_init: None,
                generation: 0,
            }))),
            updates: Arc::new(Mutex::new(crate::init::effects::InitEffects::default())),
            is_sandbox,
            access_token: RwLock::new(None),
        }
    }

    pub async fn snapshot(&self) -> Arc<RuntimeState> {
        self.current.read().await.clone()
    }
}

impl Default for RuntimeStateStore {
    fn default() -> Self {
        Self::new()
    }
}

impl crate::server::ReadinessCheck for RuntimeStateStore {
    fn self_check(&self) -> futures::future::BoxFuture<'_, bool> {
        Box::pin(async move {
            let state = self.current.read().await;
            // Wire defaults are opaque strings, as in the oracle. Process/file
            // operations validate them before using OS credentials or C strings.
            state.generation < u64::MAX
        })
    }
}

#[derive(Debug, Clone)]
pub struct UserDatabase {
    passwd_path: PathBuf,
}

impl UserDatabase {
    pub fn system() -> Self {
        Self {
            passwd_path: PathBuf::from("/etc/passwd"),
        }
    }

    pub fn from_passwd_file(path: impl AsRef<Path>) -> Self {
        Self {
            passwd_path: path.as_ref().to_owned(),
        }
    }

    async fn lookup(&self, username: &str) -> Result<Option<Vec<String>>, DomainError> {
        let passwd = tokio::fs::read_to_string(&self.passwd_path)
            .await
            .map_err(|_| DomainError::Internal)?;
        Ok(passwd
            .lines()
            .find(|line| {
                line.split_once(':')
                    .is_some_and(|(candidate, _)| candidate == username)
            })
            .map(|line| line.split(':').map(str::to_owned).collect()))
    }

    pub(crate) async fn validate_username(&self, username: &str) -> Result<(), DomainError> {
        if !valid_username(username) {
            return Err(DomainError::InvalidArgument("invalid target user".into()));
        }
        let fields = self
            .lookup(username)
            .await?
            .ok_or(DomainError::Unauthenticated)?;
        if fields.len() != 7
            || fields[2].parse::<u32>().is_err()
            || fields[3].parse::<u32>().is_err()
        {
            return Err(DomainError::Internal);
        }
        Ok(())
    }
}

fn valid_username(username: &str) -> bool {
    !username.is_empty()
        && !username
            .bytes()
            .any(|byte| matches!(byte, b':' | b'/' | b'\0' | b'\n' | b'\r'))
}
