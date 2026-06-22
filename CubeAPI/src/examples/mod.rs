// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
//! Example registry and topology templates.
//!
//! Pure-data modules with no async dependencies; they are consumed by
//! [`crate::services::examples::ExampleService`].

pub mod registry;
pub mod topology;

pub use registry::{file_languages, scenario_registry, FileSpec, ScenarioSpec};
pub use topology::{topology_with_status, TopologyGraph};
