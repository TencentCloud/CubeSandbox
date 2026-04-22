// Copyright © 2019 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//

use crate::protocol::MemoryRangeTable;
use anyhow::anyhow;
use serde::{Deserialize, Serialize};
use thiserror::Error;

pub mod protocol;

#[derive(Error, Debug)]
pub enum MigratableError {
    #[error("Failed to pause migratable component: {0}")]
    Pause(#[source] anyhow::Error),

    #[error("Failed to resume migratable component: {0}")]
    Resume(#[source] anyhow::Error),

    #[error("Failed to snapshot migratable component: {0}")]
    Snapshot(#[source] anyhow::Error),

    #[error("Failed to restore migratable component: {0}")]
    Restore(#[source] anyhow::Error),

    #[error("Failed to send migratable component snapshot: {0}")]
    MigrateSend(#[source] anyhow::Error),

    #[error("Failed to receive migratable component snapshot: {0}")]
    MigrateReceive(#[source] anyhow::Error),

    #[error("Socket error: {0}")]
    MigrateSocket(#[source] std::io::Error),

    #[error("Failed to start migration for migratable component: {0}")]
    StartDirtyLog(#[source] anyhow::Error),

    #[error("Failed to stop migration for migratable component: {0}")]
    StopDirtyLog(#[source] anyhow::Error),

    #[error("Failed to retrieve dirty ranges for migratable component: {0}")]
    DirtyLog(#[source] anyhow::Error),

    #[error("Failed to start migration for migratable component: {0}")]
    StartMigration(#[source] anyhow::Error),

    #[error("Failed to complete migration for migratable component: {0}")]
    CompleteMigration(#[source] anyhow::Error),
}

/// A Pausable component can be paused and resumed.
pub trait Pausable {
    /// Pause the component.
    fn pause(&mut self) -> std::result::Result<(), MigratableError> {
        Ok(())
    }

    /// Resume the component.
    fn resume(&mut self) -> std::result::Result<(), MigratableError> {
        Ok(())
    }
}

/// A Snapshottable component snapshot section.
/// Migratable component can split their migration snapshot into
/// separate sections.
/// Splitting a component migration data into different sections
/// allows for easier and forward compatible extensions.
#[derive(Clone, Default, Deserialize, Serialize)]
pub struct SnapshotDataSection {
    /// The section id.
    pub id: String,

    /// The section serialized snapshot.
    pub snapshot: String,
}

impl SnapshotDataSection {
    /// Generate the state data from the snapshot data
    pub fn to_state<'a, T>(&'a self) -> Result<T, MigratableError>
    where
        T: Deserialize<'a>,
    {
        serde_json::from_str(&self.snapshot).map_err(|e| {
            MigratableError::Restore(anyhow!("Error deserialising: {} {}", self.id, e))
        })
    }

    /// Create from state that can be serialized
    pub fn new_from_state<T>(id: &str, state: &T) -> Result<Self, MigratableError>
    where
        T: Serialize,
    {
        let snapshot = serde_json::to_string(state)
            .map_err(|e| MigratableError::Snapshot(anyhow!("Error serialising: {} {}", id, e)))?;

        let snapshot_data = SnapshotDataSection {
            id: format!("{}-section", id),
            snapshot,
        };

        Ok(snapshot_data)
    }
}

/// A Snapshottable component's snapshot is a tree of snapshots, where leaves
/// contain the snapshot data. Nodes of this tree track all their children
/// through the snapshots field, which is basically their sub-components.
/// Leaves will typically have an empty snapshots map, while nodes usually
/// carry an empty snapshot_data.
///
/// For example, a device manager snapshot is the composition of all its
/// devices snapshots. The device manager Snapshot would have no snapshot_data
/// but one Snapshot child per tracked device. Then each device's Snapshot
/// would carry an empty snapshots map but a map of SnapshotDataSection, i.e.
/// the actual device snapshot data.
#[derive(Clone, Default, Deserialize, Serialize)]
pub struct Snapshot {
    /// The Snapshottable component id.
    pub id: String,

    /// The Snapshottable component snapshots.
    pub snapshots: std::collections::BTreeMap<String, Box<Snapshot>>,

    /// The Snapshottable component's snapshot data.
    /// A map of snapshot sections, indexed by the section ids.
    pub snapshot_data: std::collections::HashMap<String, SnapshotDataSection>,
}

impl Snapshot {
    /// Create an empty Snapshot.
    pub fn new(id: &str) -> Self {
        Snapshot {
            id: id.to_string(),
            ..Default::default()
        }
    }

    /// Create from state that can be serialized
    pub fn new_from_state<T>(id: &str, state: &T) -> Result<Self, MigratableError>
    where
        T: Serialize,
    {
        let mut snapshot_data = Snapshot::new(id);
        snapshot_data.add_data_section(SnapshotDataSection::new_from_state(id, state)?);

        Ok(snapshot_data)
    }

    /// Add a sub-component's Snapshot to the Snapshot.
    pub fn add_snapshot(&mut self, snapshot: Snapshot) {
        self.snapshots
            .insert(snapshot.id.clone(), Box::new(snapshot));
    }

    /// Add a SnapshotDatasection to the component snapshot data.
    pub fn add_data_section(&mut self, section: SnapshotDataSection) {
        self.snapshot_data.insert(section.id.clone(), section);
    }

    /// Generate the state data from the snapshot
    pub fn to_state<'a, T>(&'a self, id: &str) -> Result<T, MigratableError>
    where
        T: Deserialize<'a>,
    {
        self.snapshot_data
            .get(&format!("{}-section", id))
            .ok_or_else(|| MigratableError::Restore(anyhow!("Missing section for {}", id)))?
            .to_state()
    }
}

pub fn snapshot_from_id(snapshot: Option<&Snapshot>, id: &str) -> Option<Snapshot> {
    snapshot.and_then(|s| s.snapshots.get(id).map(|s| *s.clone()))
}

pub fn state_from_id<'a, T>(s: Option<&'a Snapshot>, id: &str) -> Result<Option<T>, MigratableError>
where
    T: Deserialize<'a>,
{
    if let Some(s) = s.as_ref() {
        s.snapshots.get(id).map(|s| s.to_state(id)).transpose()
    } else {
        Ok(None)
    }
}

/// A snapshottable component can be snapshotted.
pub trait Snapshottable: Pausable {
    /// The snapshottable component id.
    fn id(&self) -> String {
        String::new()
    }

    /// Take a component snapshot.
    fn snapshot(&mut self) -> std::result::Result<Snapshot, MigratableError> {
        Ok(Snapshot::new(""))
    }

    /// Restore a component from its snapshot.
    fn restore(&mut self, _snapshot: Snapshot) -> std::result::Result<(), MigratableError> {
        Ok(())
    }
}

/// A transportable component can be sent or receive to a specific URL.
///
/// This trait is meant to be used for component that have custom
/// transport handlers.
pub trait Transportable: Pausable + Snapshottable {
    /// Send a component snapshot.
    ///
    /// # Arguments
    ///
    /// * `snapshot` - The migratable component snapshot to send.
    /// * `destination_url` - The destination URL to send the snapshot to. This
    ///                       could be an HTTP endpoint, a TCP address or a local file.
    fn send(
        &self,
        _snapshot: &Snapshot,
        _destination_url: &str,
    ) -> std::result::Result<(), MigratableError> {
        Ok(())
    }

    /// Receive a component snapshot.
    ///
    /// # Arguments
    ///
    /// * `source_url` - The source URL to fetch the snapshot from. This could be an HTTP
    ///                  endpoint, a TCP address or a local file.
    fn recv(&self, _source_url: &str) -> std::result::Result<Snapshot, MigratableError> {
        Ok(Snapshot::new(""))
    }
}

/// Trait to be implemented by any component (device, CPU, RAM, etc) that
/// can be migrated.
/// All migratable components are paused before being snapshotted, and then
/// eventually resumed. Thus any Migratable component must be both Pausable
/// and Snapshottable.
/// Moreover a migratable component can be transported to a remote or local
/// destination and thus must be Transportable.
pub trait Migratable: Send + Pausable + Snapshottable + Transportable {
    fn start_dirty_log(&mut self) -> std::result::Result<(), MigratableError> {
        Ok(())
    }

    fn stop_dirty_log(&mut self) -> std::result::Result<(), MigratableError> {
        Ok(())
    }

    fn dirty_log(&mut self) -> std::result::Result<MemoryRangeTable, MigratableError> {
        Ok(MemoryRangeTable::default())
    }

    fn start_migration(&mut self) -> std::result::Result<(), MigratableError> {
        Ok(())
    }

    fn complete_migration(&mut self) -> std::result::Result<(), MigratableError> {
        Ok(())
    }
}
