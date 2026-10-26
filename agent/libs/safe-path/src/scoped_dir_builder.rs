// Copyright (c) 2022 Alibaba Cloud
//
// SPDX-License-Identifier: Apache-2.0
//

use std::io::{Error, ErrorKind, Result};
use std::path::Path;

use crate::{scoped_join, scoped_resolve, PinnedPathBuf};

const DIRECTORY_MODE_DEFAULT: u32 = 0o777;
const DIRECTORY_MODE_MASK: u32 = 0o777;

/// Safe version of `DirBuilder` to protect from TOCTOU style of attacks.
///
/// The `ScopedDirBuilder` is a counterpart for `DirBuilder`, with safety enhancements of:
/// - ensuring the new directories are created under a specified `root` directory.
/// - ensuring all created directories are still scoped under `root` even under symlink based
///   attacks.
/// - returning a [PinnedPathBuf] for the last level of directory, so it could be used for other
///   operations safely.
#[derive(Debug)]
pub struct ScopedDirBuilder {
    root: PinnedPathBuf,
    mode: u32,
    recursive: bool,
}

impl ScopedDirBuilder {
    /// Create a new instance of `ScopedDirBuilder` with with default mode/security settings.
    pub fn new<P: AsRef<Path>>(root: P) -> Result<Self> {
        let root = root.as_ref().canonicalize()?;
        let root = PinnedPathBuf::from_path(root)?;
        if !root.metadata()?.is_dir() {
            return Err(Error::new(
                ErrorKind::Other,
                format!("Invalid root path: {}", root.display()),
            ));
        }

        Ok(ScopedDirBuilder {
            root,
            mode: DIRECTORY_MODE_DEFAULT,
            recursive: false,
        })
    }

    /// Indicates that directories should be created recursively, creating all parent directories.
    ///
    /// Parents that do not exist are created with the same security and permissions settings.
    pub fn recursive(&mut self, recursive: bool) -> &mut Self {
        self.recursive = recursive;
        self
    }

    /// Sets the mode to create new directories with. This option defaults to 0o755.
    pub fn mode(&mut self, mode: u32) -> &mut Self {
        self.mode = mode & DIRECTORY_MODE_MASK;
        self
    }

    /// Creates the specified directory with the options configured in this builder.
    ///
    /// This is a helper to create subdirectory with an absolute path, without stripping off
    /// `self.root`. So error will be returned if path does start with `self.root`.
    /// It is considered an error if the directory already exists unless recursive mode is enabled.
    pub fn create_with_unscoped_path<P: AsRef<Path>>(&self, path: P) -> Result<PinnedPathBuf> {
        if !path.as_ref().is_absolute() {
            return Err(Error::new(
                ErrorKind::Other,
                format!(
                    "Expected absolute directory path: {}",
                    path.as_ref().display()
                ),
            ));
        }
        // Partially canonicalize `path` so we can strip the `root` part.
        let scoped_path = scoped_join("/", path)?;
        let stripped_path = scoped_path.strip_prefix(self.root.target()).map_err(|_| {
            Error::new(
                ErrorKind::Other,
                format!(
                    "Path {} is not under {}",
                    scoped_path.display(),
                    self.root.target().display()
                ),
            )
        })?;

        self.do_mkdir(stripped_path)
    }

    /// Creates sub-directory with the options configured in this builder.
    ///
    /// It is considered an error if the directory already exists unless recursive mode is enabled.
    pub fn create<P: AsRef<Path>>(&self, path: P) -> Result<PinnedPathBuf> {
        let path = scoped_resolve(&self.root, path)?;
        self.do_mkdir(&path)
    }

    fn do_mkdir(&self, path: &Path) -> Result<PinnedPathBuf> {
        assert!(path.is_relative());
        if path.file_name().is_none() {
            if !self.recursive {
                return Err(Error::new(
                    ErrorKind::AlreadyExists,
                    "directory already exists",
                ));
            } else {
                return self.root.try_clone();
            }
        }

        // Safe because `path` have at least one level.
        let levels = path.iter().count() - 1;
        let mut dir = self.root.try_clone()?;
        for (idx, comp) in path.iter().enumerate() {
            match dir.open_child(comp) {
                Ok(v) => {
                    if !v.metadata()?.is_dir() {
                        return Err(Error::new(
                            ErrorKind::Other,
                            format!("Path {} is not a directory", v.display()),
                        ));
                    } else if !self.recursive && idx == levels {
                        return Err(Error::new(
                            ErrorKind::AlreadyExists,
                            "directory already exists",
                        ));
                    }
                    dir = v;
                }
                Err(_e) => {
                    if !self.recursive && idx != levels {
                        return Err(Error::new(
                            ErrorKind::NotFound,
                            "parent directory does not exist".to_string(),
                        ));
                    }
                    dir = dir.mkdir(comp, self.mode)?;
                }
            }
        }

        Ok(dir)
    }
}

#[allow(clippy::zero_prefixed_literal)]
#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;
    use std::fs::DirBuilder;
    use std::os::unix::fs::{symlink, MetadataExt};
    use tempfile::tempdir;

    #[test]
    fn test_scoped_dir_builder() {
        // create temporary directory to emulate container rootfs with symlink
        let rootfs_dir = tempdir().expect("failed to create tmpdir");
        DirBuilder::new()
            .create(rootfs_dir.path().join("b"))
            .unwrap();
        symlink(rootfs_dir.path().join("b"), rootfs_dir.path().join("a")).unwrap();
        let rootfs_path = &rootfs_dir.path().join("a");

        // root directory doesn't exist
        ScopedDirBuilder::new(rootfs_path.join("__does_not_exist__")).unwrap_err();
        ScopedDirBuilder::new("__does_not_exist__").unwrap_err();

        // root is a file
        fs::write(rootfs_path.join("txt"), "test").unwrap();
        ScopedDirBuilder::new(rootfs_path.join("txt")).unwrap_err();

        let mut builder = ScopedDirBuilder::new(&rootfs_path).unwrap();

        // file with the same name already exists.
        builder
            .create_with_unscoped_path(rootfs_path.join("txt"))
            .unwrap_err();
        // parent is a file
        builder.create("/txt/a").unwrap_err();
        // Not starting with root
        builder.create_with_unscoped_path("/txt/a").unwrap_err();
        // creating "." without recursive mode should fail
        builder
            .create_with_unscoped_path(rootfs_path.join("."))
            .unwrap_err();
        // parent doesn't exist
        builder
            .create_with_unscoped_path(rootfs_path.join("a/b"))
            .unwrap_err();
        builder.create("a/b/c").unwrap_err();

        let path = builder.create("a").unwrap();
        assert!(rootfs_path.join("a").is_dir());
        assert_eq!(path.target(), rootfs_path.join("a").canonicalize().unwrap());

        // Creating an existing directory without recursive mode should fail.
        builder
            .create_with_unscoped_path(rootfs_path.join("a"))
            .unwrap_err();

        // Creating an existing directory with recursive mode should succeed.
        builder.recursive(true);
        let path = builder
            .create_with_unscoped_path(rootfs_path.join("a"))
            .unwrap();
        assert_eq!(path.target(), rootfs_path.join("a").canonicalize().unwrap());
        let path = builder.create(".").unwrap();
        assert_eq!(path.target(), rootfs_path.canonicalize().unwrap());

        let umask = unsafe { libc::umask(0022) };
        unsafe { libc::umask(umask) };

        builder.mode(0o740);
        let path = builder.create("a/b/c/d").unwrap();
        assert_eq!(
            path.target(),
            rootfs_path.join("a/b/c/d").canonicalize().unwrap()
        );
        assert!(rootfs_path.join("a/b/c/d").is_dir());
        assert_eq!(
            rootfs_path.join("a").metadata().unwrap().mode() & 0o777,
            DIRECTORY_MODE_DEFAULT & !umask,
        );
        assert_eq!(
            rootfs_path.join("a/b").metadata().unwrap().mode() & 0o777,
            0o740 & !umask
        );
        assert_eq!(
            rootfs_path.join("a/b/c").metadata().unwrap().mode() & 0o777,
            0o740 & !umask
        );
        assert_eq!(
            rootfs_path.join("a/b/c/d").metadata().unwrap().mode() & 0o777,
            0o740 & !umask
        );

        // Creating should fail if some components are not directory.
        builder.create("txt/e/f").unwrap_err();
        fs::write(rootfs_path.join("a/b/txt"), "test").unwrap();
        builder.create("a/b/txt/h/i").unwrap_err();
    }

    #[test]
    fn test_create_root() {
        let mut builder = ScopedDirBuilder::new("/").unwrap();
        builder.recursive(true);
        builder.create("/").unwrap();
        builder.create(".").unwrap();
        builder.create("..").unwrap();
        builder.create("../../.").unwrap();
        builder.create("").unwrap();
        builder.create_with_unscoped_path("/").unwrap();
        builder.create_with_unscoped_path("/..").unwrap();
        builder.create_with_unscoped_path("/../.").unwrap();
    }

    #[test]
    fn test_create_with_absolute_path() {
        // create temporary directory to emulate container rootfs with symlink
        let rootfs_dir = tempdir().expect("failed to create tmpdir");
        DirBuilder::new()
            .create(rootfs_dir.path().join("b"))
            .unwrap();
        symlink(rootfs_dir.path().join("b"), rootfs_dir.path().join("a")).unwrap();
        let rootfs_path = &rootfs_dir.path().join("a");

        let mut builder = ScopedDirBuilder::new(&rootfs_path).unwrap();
        builder.create_with_unscoped_path("/").unwrap_err();
        builder
            .create_with_unscoped_path(rootfs_path.join("../__xxxx___xxx__"))
            .unwrap_err();
        builder
            .create_with_unscoped_path(rootfs_path.join("c/d"))
            .unwrap_err();

        // Return `AlreadyExist` when recursive is false
        builder.create_with_unscoped_path(&rootfs_path).unwrap_err();
        builder
            .create_with_unscoped_path(rootfs_path.join("."))
            .unwrap_err();

        builder.recursive(true);
        builder.create_with_unscoped_path(&rootfs_path).unwrap();
        builder
            .create_with_unscoped_path(rootfs_path.join("."))
            .unwrap();
        builder
            .create_with_unscoped_path(rootfs_path.join("c/d"))
            .unwrap();
    }
}
