// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! Security primitives for CubeAPI.
//!
//! The `outbound_url` module provides SSRF-resistant URL validation,
//! DNS pinning, and hardened HTTP client construction for any feature
//! that needs to call an external URL (auth callbacks, webhooks, etc.).

pub mod outbound_url;

#[allow(unused_imports)]
pub use outbound_url::{
    build_secure_client, OutboundUrlError, OutboundUrlPolicy, OutboundUrlSecurityConfig,
    ValidatedUrl,
};

#[cfg(feature = "webhooks")]
#[allow(unused_imports)]
pub use outbound_url::{read_body_with_limit, BodyLimitError};
