// Copyright (c) 2022 Alibaba Cloud
//
// SPDX-License-Identifier: Apache-2.0
//

//! A library to safely handle filesystem paths, typically for container runtimes.
//!
//! Linux [mount namespace](https://man7.org/linux/man-pages/man7/mount_namespaces.7.html)
//! provides isolation of the list of mounts seen by the processes in each
//! [namespace](https://man7.org/linux/man-pages/man7/namespaces.7.html) instance.
//! Thus, the processes in each of the mount namespace instances will see distinct single-directory
//! hierarchies.
//!
//! Containers are used to isolate workloads from the host system. Container on Linux systems
//! depends on the mount namespace to build an isolated root filesystem for each container,
//! thus protect the host and containers from each other. When creating containers, the container
//! runtime needs to setup filesystem mounts for container rootfs/volumes. Configuration for
//! mounts/paths may be indirectly controlled by end users through:
//! - container images
//! - Kubernetes pod specifications
//! - hook command line arguments
//!
//! These volume configuration information may be controlled by end users/malicious attackers,
//! so it must not be trusted by container runtimes. When the container runtime is preparing mount
//! namespace for a container, it must be very careful to validate user input configuration
//! information and ensure data out of the container rootfs directory won't be affected
//! by the container. There are several types of attacks related to container mount namespace:
//! - symlink based attack
//! - Time of check to time of use (TOCTTOU)
//!
//! This crate provides several mechanisms for container runtimes to safely handle filesystem paths
//! when preparing mount namespace for containers.
//! - [scoped_join()](crate::scoped_join()): safely join `unsafe_path` to `root`, and ensure
//!   `unsafe_path` is scoped under `root`.
//! - [scoped_resolve()](crate::scoped_resolve()): resolve `unsafe_path` to a relative path,
//!   rooted at and constrained by `root`.
//! - [struct PinnedPathBuf](crate::PinnedPathBuf): safe version of `PathBuf` to protect from
//!   TOCTTOU style of attacks, which ensures:
//!     - the value of [`PinnedPathBuf::as_path()`] never changes.
//!     - the path returned by [`PinnedPathBuf::as_path()`] is always a symlink.
//!     - the filesystem object referenced by the symlink [`PinnedPathBuf::as_path()`] never changes.
//!     - the value of [`PinnedPathBuf::target()`] never changes.
//! - [struct ScopedDirBuilder](crate::ScopedDirBuilder): safe version of `DirBuilder` to protect
//!   from symlink race and TOCTTOU style of attacks, which enhances security by:
//!     - ensuring the new directories are created under a specified `root` directory.
//!     - avoiding symlink race attacks during making directories.
//!     - returning a [PinnedPathBuf] for the last level of directory, so it could be used for other
//!       operations safely.
//!
//! The work is inspired by:
//! - [`filepath-securejoin`](https://github.com/cyphar/filepath-securejoin): secure_join() written
//! in Go.
//! - [CVE-2021-30465](https://github.com/advisories/GHSA-c3xm-pvg7-gh7r): symlink related TOCTOU
//! flaw in `runC`.

#![deny(missing_docs)]

mod pinned_path_buf;
pub use pinned_path_buf::PinnedPathBuf;

mod scoped_dir_builder;
pub use scoped_dir_builder::ScopedDirBuilder;

mod scoped_path_resolver;
pub use scoped_path_resolver::{scoped_join, scoped_resolve};
