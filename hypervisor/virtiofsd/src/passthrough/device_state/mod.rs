// Copyright 2024 Red Hat, Inc. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

/**
 * Module for migrating our internal FS state (i.e. serializing and deserializing it), with the
 * following submodules:
 * - serialized: Serialized data structures
 * - preserialization: Structures and functionality for preparing for migration (serialization),
 *                     i.e. define and construct the precursors to the eventually serialized
 *                     information that are stored alongside the associated inodes and handles they
 *                     describe
 * - serialization: Functionality for serializing
 * - deserialization: Functionality for deserializing
 */
mod deserialization;
pub(super) mod preserialization;
mod serialization;
mod serialized;

use crate::filesystem::SerializableFileSystem;
use crate::passthrough::PassthroughFs;
use preserialization::{InodeMigrationInfoConstructor, PathReconstructor};
use std::convert::{TryFrom, TryInto};
use std::fs::File;
use std::io::{self, Read, Write};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;

/// Adds serialization (migration) capabilities to `PassthroughFs`
impl SerializableFileSystem for PassthroughFs {
    fn prepare_serialization(&self, cancel: Arc<AtomicBool>) -> io::Result<()> {
        self.inodes.clear_migration_info();

        // Set this so the filesystem code knows that every node is supposed to have up-to-date
        // migration information.  For example, nodes that are created after they would have been
        // visited by the reconstructor below will not get migration info, unless the general
        // filesystem code makes an effort to set it (when the node is created).
        self.track_migration_info.store(true, Ordering::Relaxed);

        // Create the reconstructor (which reconstructs parent+filename information for each node
        // in our inode store), and run it
        let reconstructor = PathReconstructor::new(self, cancel);
        let result = reconstructor.execute();
        if result.is_err() {
            // Do not leave incomplete data behind (cancelling returns an error, too, landing here)
            self.inodes.clear_migration_info();
        }
        result
    }

    fn serialize_data(&self) -> io::Result<Vec<u8>> {
        self.track_migration_info.store(false, Ordering::Relaxed);

        let result = (|| {
            let state = serialized::PassthroughFs::V1(self.try_into()?);
            let serialized: Vec<u8> = state.try_into()?;
            Ok(serialized)
        })();

        self.inodes.clear_migration_info();
        result
    }

    fn serialize(&self, mut state_pipe: File) -> io::Result<()> {
        let serialized: Vec<u8> = self.serialize_data()?;
        state_pipe.write_all(&serialized)?;
        Ok(())
    }

    fn deserialize_and_apply_data(&self, serialized: &Vec<u8>) -> io::Result<()> {
        match serialized::PassthroughFs::try_from(serialized)? {
            serialized::PassthroughFs::V1(state) => state.apply(self)?,
        };
        Ok(())
    }

    fn deserialize_and_apply(&self, mut state_pipe: File) -> io::Result<()> {
        let mut serialized: Vec<u8> = Vec::new();
        state_pipe.read_to_end(&mut serialized)?;
        self.deserialize_and_apply_data(&serialized)
    }
}

#[cfg(test)]
mod tests {
    use super::preserialization::{InodeLocation, InodeMigrationInfo};
    use super::serialized;
    use crate::filesystem::SerializableFileSystem;
    use crate::fuse;
    use crate::passthrough::file_handle::FileOrHandle;
    use crate::passthrough::inode_store::{InodeData, InodeIds};
    use crate::passthrough::{Config, PassthroughFs};
    use std::convert::TryFrom;
    use std::fs::{self, File};
    use std::path::{Path, PathBuf};
    use std::sync::atomic::AtomicU64;
    use std::sync::Mutex;
    use std::time::{SystemTime, UNIX_EPOCH};

    struct TestDir {
        path: PathBuf,
    }

    impl TestDir {
        fn new(name: &str) -> Self {
            let unique = SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap()
                .as_nanos();
            let path = std::env::temp_dir().join(format!(
                "virtiofsd-device-state-{name}-{}-{unique}",
                std::process::id()
            ));
            fs::create_dir_all(&path).unwrap();
            Self { path }
        }

        fn path(&self) -> &Path {
            &self.path
        }
    }

    impl Drop for TestDir {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.path);
        }
    }

    fn new_passthrough_fs(root: &Path) -> PassthroughFs {
        PassthroughFs::new(Config {
            root_dir: root.to_string_lossy().into_owned(),
            ..Default::default()
        })
        .unwrap()
    }

    fn set_root_migration_info(fs: &PassthroughFs) {
        let root = fs.inodes.get(fuse::ROOT_ID).unwrap();
        *root.migration_info.lock().unwrap() = Some(InodeMigrationInfo {
            location: InodeLocation::RootNode,
            file_handle: None,
        });
    }

    #[test]
    fn serialize_data_marks_non_root_inode_invalid_when_location_is_missing() {
        let test_dir = TestDir::new("missing-child-info");
        let child_path = test_dir.path().join("child");
        fs::write(&child_path, b"child").unwrap();

        let fs = new_passthrough_fs(test_dir.path());
        fs.open_root_node().unwrap();
        set_root_migration_info(&fs);
        fs.inodes
            .new_inode(InodeData {
                inode: 2,
                file_or_handle: FileOrHandle::File(File::open(&child_path).unwrap()),
                refcount: AtomicU64::new(1),
                ids: InodeIds {
                    ino: 2,
                    dev: 3,
                    mnt_id: 4,
                },
                mode: libc::S_IFREG as u32,
                migration_info: Mutex::new(None),
            })
            .unwrap();

        let data = fs.serialize_data().unwrap();
        let serialized = serialized::PassthroughFs::try_from(&data).unwrap();
        let serialized::PassthroughFs::V1(state) = serialized;

        let child = state.inodes.iter().find(|inode| inode.id == 2).unwrap();
        assert!(matches!(child.location, serialized::InodeLocation::Invalid));
        let root = state
            .inodes
            .iter()
            .find(|inode| inode.id == fuse::ROOT_ID)
            .unwrap();
        assert!(matches!(root.location, serialized::InodeLocation::RootNode));
        let root = fs.inodes.get(fuse::ROOT_ID).unwrap();
        assert!(root.migration_info.lock().unwrap().is_none());
    }
}
