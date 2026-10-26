// Copyright 2024 Red Hat, Inc. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

/// Structs and enums that constitute our serialized state "on the wire".  Turning them into/from
/// plain bytes still needs to be done with some serde implementation.
use crate::passthrough::file_handle::SerializableFileHandle;
use crate::passthrough::inode_store::Inode as InodeId;
use crate::passthrough::Handle as HandleId;
use serde::{Deserialize, Serialize};

/// Full serialized device state (for `PassthroughFs`).  This is an enum so in case incompatible
/// changes have to be made, new version variants can be added while still being able to migrate
/// from older versions.
#[derive(Debug, Deserialize, Serialize)]
pub(super) enum PassthroughFs {
    /// Initial version
    V1(PassthroughFsV1),
}

#[derive(Debug, Deserialize, Serialize)]
pub(super) struct PassthroughFsV1 {
    /// List of all looked up inodes
    pub(super) inodes: Vec<Inode>,
    /// Next free index for inode IDs
    pub(super) next_inode: u64,

    /// List of all open files (handles)
    pub(super) handles: Vec<Handle>,
    /// Next free index for handle IDs
    pub(super) next_handle: u64,

    /// Remember which options have been negotiated during INIT
    pub(super) negotiated_opts: NegotiatedOpts,
}

/// Options that can be negotiated during INIT, i.e. ones for which we must remember whether we
/// have enabled them after negotiating with the guest
#[derive(Debug, Deserialize, Serialize)]
pub(super) struct NegotiatedOpts {
    pub(super) writeback: bool,
    pub(super) announce_submounts: bool,
    pub(super) posix_acl: bool,
    pub(super) sup_group_extension: bool,
}

/// Serializable data for an inode that has been looked up
#[derive(Debug, Deserialize, Serialize)]
pub(super) struct Inode {
    /// Own inode ID
    pub(super) id: InodeId,

    /// Current refcount
    pub(super) refcount: u64,

    /// Description of this inode that allows the destination to find it
    pub(super) location: InodeLocation,

    /// Inode file handle.  If present, the destination is not supposed to open this file handle,
    /// but instead compare it against the one of the inode it has opened based on `location`.
    pub(super) file_handle: Option<SerializableFileHandle>,
}

/// Serializable description of some inode that allows the destination to find it
#[derive(Debug, Deserialize, Serialize)]
pub(super) enum InodeLocation {
    /// The root node is not given a serialized location; the destination is supposed to find it on
    /// its own
    RootNode,

    /// Described by its path: The destination will have to open the given filename
    Path {
        /// ID of the parent inode
        parent: InodeId,

        /// A filename relative to the parent that allows opening this inode.  Note that using
        /// `String` restricts us to paths that can be represented as UTF-8, which is not
        /// necessarily a restriction that all operating systems have.  However, we need to use
        /// some common encoding (i.e., cannot use `OsString`), or otherwise we could not migrate
        /// between operating systems using different string representations.
        filename: String,
        fullname: String,
    },

    /// Source has deemed that this inode can no longer be found.  The destination needs to decide
    /// how to proceed (e.g. whether to abort migration or simply remember that this inode is
    /// invalid and tell the guest so).
    Invalid,

    /// Described by its path: The destination will have to open the given filename relative to the
    /// shared directory (the root node).  In contrast to `Path`, there is no strong reference to
    /// the shared directory node.
    FullPath {
        /// Filename relative to the shared directory root node.  Stored in UTF-8, just like
        /// `Path.filename`.
        filename: String,
    },
}

/// Serializable representation of an open file (a handle)
#[derive(Debug, Deserialize, Serialize)]
pub(super) struct Handle {
    /// Own handle ID
    pub(super) id: HandleId,

    /// Inode to which this handle refers
    pub(super) inode: InodeId,

    /// Describes where this handle comes from, so the destination can open it
    pub(super) source: HandleSource,
}

/// Serializable description of some handle that allows the destination to open it
#[derive(Debug, Deserialize, Serialize)]
pub(super) enum HandleSource {
    /// Handle should be opened by opening `Handle.inode` with the `open(2)` flags given here
    OpenInode {
        /// Flags passed to `openat(2)`
        flags: i32,
    },
}
