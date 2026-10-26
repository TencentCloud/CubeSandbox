// Copyright (c) 2022 Alibaba Cloud
//
// SPDX-License-Identifier: Apache-2.0
//

use std::io::{Error, ErrorKind, Result};
use std::path::{Component, Path, PathBuf};

// Follow the same configuration as
// [secure_join](https://github.com/cyphar/filepath-securejoin/blob/master/join.go#L51)
const MAX_SYMLINK_DEPTH: u32 = 255;

fn do_scoped_resolve<R: AsRef<Path>, U: AsRef<Path>>(
    root: R,
    unsafe_path: U,
) -> Result<(PathBuf, PathBuf)> {
    let root = root.as_ref().canonicalize()?;

    let mut nlinks = 0u32;
    let mut curr_path = unsafe_path.as_ref().to_path_buf();
    'restart: loop {
        let mut subpath = PathBuf::new();
        let mut iter = curr_path.components();

        'next_comp: while let Some(comp) = iter.next() {
            match comp {
                // Linux paths don't have prefixes.
                Component::Prefix(_) => {
                    return Err(Error::new(
                        ErrorKind::Other,
                        format!("Invalid path prefix in: {}", unsafe_path.as_ref().display()),
                    ));
                }
                // `RootDir` should always be the first component, and Path::components() ensures
                // that.
                Component::RootDir | Component::CurDir => {
                    continue 'next_comp;
                }
                Component::ParentDir => {
                    subpath.pop();
                }
                Component::Normal(n) => {
                    let path = root.join(&subpath).join(n);
                    if let Ok(v) = path.read_link() {
                        nlinks += 1;
                        if nlinks > MAX_SYMLINK_DEPTH {
                            return Err(Error::new(
                                ErrorKind::Other,
                                format!(
                                    "Too many levels of symlinks: {}",
                                    unsafe_path.as_ref().display()
                                ),
                            ));
                        }
                        curr_path = if v.is_absolute() {
                            v.join(iter.as_path())
                        } else {
                            subpath.join(v).join(iter.as_path())
                        };
                        continue 'restart;
                    } else {
                        subpath.push(n);
                    }
                }
            }
        }

        return Ok((root, subpath));
    }
}

/// Resolve `unsafe_path` to a relative path, rooted at and constrained by `root`.
///
/// The `scoped_resolve()` function assumes `root` exists and is an absolute path. It processes
/// each path component in `unsafe_path` as below:
/// - assume it's not a symlink and output if the component doesn't exist yet.
/// - ignore if it's "/" or ".".
/// - go to parent directory but constrained by `root` if it's "..".
/// - recursively resolve to the real path if it's a symlink. All symlink resolutions will be
///   constrained by `root`.
/// - otherwise output the path component.
///
/// # Arguments
/// - `root`: the absolute path to constrain the symlink resolution.
/// - `unsafe_path`: the path to resolve.
///
/// Note that the guarantees provided by this function only apply if the path components in the
/// returned PathBuf are not modified (in other words are not replaced with symlinks on the
/// filesystem) after this function has returned. You may use [crate::PinnedPathBuf] to protect
/// from such TOCTOU attacks.
pub fn scoped_resolve<R: AsRef<Path>, U: AsRef<Path>>(root: R, unsafe_path: U) -> Result<PathBuf> {
    do_scoped_resolve(root, unsafe_path).map(|(_root, path)| path)
}

/// Safely join `unsafe_path` to `root`, and ensure `unsafe_path` is scoped under `root`.
///
/// The `scoped_join()` function assumes `root` exists and is an absolute path. It safely joins the
/// two given paths and ensures:
/// - The returned path is guaranteed to be scoped inside `root`.
/// - Any symbolic links in the path are evaluated with the given `root` treated as the root of the
///   filesystem, similar to a chroot.
///
/// It's modelled after [secure_join](https://github.com/cyphar/filepath-securejoin), but only
/// for Linux systems.
///
/// # Arguments
/// - `root`: the absolute path to scope the symlink evaluation.
/// - `unsafe_path`: the path to evaluated and joint with `root`. It is unsafe since it may try to
///   escape from the `root` by using "../" or symlinks.
///
/// # Security
/// On success return, the `scoped_join()` function guarantees that:
/// - The resulting PathBuf must be a child path of `root` and will not contain any symlink path
///   components (they will all get expanded).
/// - When expanding symlinks, all symlink path components must be resolved relative to the provided
///   `root`. In particular, this can be considered a userspace implementation of how chroot(2)
///    operates on file paths.
/// - Non-existent path components are unaffected.
///
/// Note that the guarantees provided by this function only apply if the path components in the
/// returned string are not modified (in other words are not replaced with symlinks on the
/// filesystem) after this function has returned. You may use [crate::PinnedPathBuf] to protect
/// from such TOCTTOU attacks.
pub fn scoped_join<R: AsRef<Path>, U: AsRef<Path>>(root: R, unsafe_path: U) -> Result<PathBuf> {
    do_scoped_resolve(root, unsafe_path).map(|(root, path)| root.join(path))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs::DirBuilder;
    use std::os::unix::fs;
    use tempfile::tempdir;

    #[allow(dead_code)]
    #[derive(Debug)]
    struct TestData<'a> {
        name: &'a str,
        rootfs: &'a Path,
        unsafe_path: &'a str,
        result: &'a str,
    }

    fn exec_tests(tests: &[TestData]) {
        for (i, t) in tests.iter().enumerate() {
            // Create a string containing details of the test
            let msg = format!("test[{}]: {:?}", i, t);
            let result = scoped_resolve(t.rootfs, t.unsafe_path).unwrap();
            let msg = format!("{}, result: {:?}", msg, result);

            // Perform the checks
            assert_eq!(&result, Path::new(t.result), "{}", msg);
        }
    }

    #[test]
    fn test_scoped_resolve() {
        // create temporary directory to emulate container rootfs with symlink
        let rootfs_dir = tempdir().expect("failed to create tmpdir");
        DirBuilder::new()
            .create(rootfs_dir.path().join("b"))
            .unwrap();
        fs::symlink(rootfs_dir.path().join("b"), rootfs_dir.path().join("a")).unwrap();
        let rootfs_path = &rootfs_dir.path().join("a");

        let tests = [
            TestData {
                name: "normal path",
                rootfs: rootfs_path,
                unsafe_path: "a/b/c",
                result: "a/b/c",
            },
            TestData {
                name: "path with .. at beginning",
                rootfs: rootfs_path,
                unsafe_path: "../../../a/b/c",
                result: "a/b/c",
            },
            TestData {
                name: "path with complex .. pattern",
                rootfs: rootfs_path,
                unsafe_path: "../../../a/../../b/../../c",
                result: "c",
            },
            TestData {
                name: "path with .. in middle",
                rootfs: rootfs_path,
                unsafe_path: "/usr/bin/../../bin/ls",
                result: "bin/ls",
            },
            TestData {
                name: "path with . and ..",
                rootfs: rootfs_path,
                unsafe_path: "/usr/./bin/../../bin/./ls",
                result: "bin/ls",
            },
            TestData {
                name: "path with . at end",
                rootfs: rootfs_path,
                unsafe_path: "/usr/./bin/../../bin/./ls/.",
                result: "bin/ls",
            },
            TestData {
                name: "path try to escape by ..",
                rootfs: rootfs_path,
                unsafe_path: "/usr/./bin/../../../../bin/./ls/../ls",
                result: "bin/ls",
            },
            TestData {
                name: "path with .. at the end",
                rootfs: rootfs_path,
                unsafe_path: "/usr/./bin/../../bin/./ls/..",
                result: "bin",
            },
            TestData {
                name: "path ..",
                rootfs: rootfs_path,
                unsafe_path: "..",
                result: "",
            },
            TestData {
                name: "path .",
                rootfs: rootfs_path,
                unsafe_path: ".",
                result: "",
            },
            TestData {
                name: "path /",
                rootfs: rootfs_path,
                unsafe_path: "/",
                result: "",
            },
            TestData {
                name: "empty path",
                rootfs: rootfs_path,
                unsafe_path: "",
                result: "",
            },
        ];

        exec_tests(&tests);
    }

    #[test]
    fn test_scoped_resolve_invalid() {
        scoped_resolve("./root_is_not_absolute_path", ".").unwrap_err();
        scoped_resolve("C:", ".").unwrap_err();
        scoped_resolve(r#"\\server\test"#, ".").unwrap_err();
        scoped_resolve(r#"http://localhost/test"#, ".").unwrap_err();
        // Chinese Unicode characters
        scoped_resolve(r#"您好"#, ".").unwrap_err();
    }

    #[test]
    fn test_scoped_resolve_symlink() {
        // create temporary directory to emulate container rootfs with symlink
        let rootfs_dir = tempdir().expect("failed to create tmpdir");
        let rootfs_path = &rootfs_dir.path();
        std::fs::create_dir(rootfs_path.join("symlink_dir")).unwrap();

        fs::symlink("../../../", rootfs_path.join("1")).unwrap();
        let tests = [TestData {
            name: "relative symlink beyond root",
            rootfs: rootfs_path,
            unsafe_path: "1",
            result: "",
        }];
        exec_tests(&tests);

        fs::symlink("/dddd", rootfs_path.join("2")).unwrap();
        let tests = [TestData {
            name: "abs symlink pointing to non-exist directory",
            rootfs: rootfs_path,
            unsafe_path: "2",
            result: "dddd",
        }];
        exec_tests(&tests);

        fs::symlink("/", rootfs_path.join("3")).unwrap();
        let tests = [TestData {
            name: "abs symlink pointing to /",
            rootfs: rootfs_path,
            unsafe_path: "3",
            result: "",
        }];
        exec_tests(&tests);

        fs::symlink("usr/bin/../bin/ls", rootfs_path.join("4")).unwrap();
        let tests = [TestData {
            name: "symlink with one ..",
            rootfs: rootfs_path,
            unsafe_path: "4",
            result: "usr/bin/ls",
        }];
        exec_tests(&tests);

        fs::symlink("usr/bin/../../bin/ls", rootfs_path.join("5")).unwrap();
        let tests = [TestData {
            name: "symlink with two ..",
            rootfs: rootfs_path,
            unsafe_path: "5",
            result: "bin/ls",
        }];
        exec_tests(&tests);

        fs::symlink(
            "../usr/bin/../../../bin/ls",
            rootfs_path.join("symlink_dir/6"),
        )
        .unwrap();
        let tests = [TestData {
            name: "symlink try to escape",
            rootfs: rootfs_path,
            unsafe_path: "symlink_dir/6",
            result: "bin/ls",
        }];
        exec_tests(&tests);

        // Detect symlink loop.
        fs::symlink("/endpoint_b", rootfs_path.join("endpoint_a")).unwrap();
        fs::symlink("/endpoint_a", rootfs_path.join("endpoint_b")).unwrap();
        scoped_resolve(rootfs_path, "endpoint_a").unwrap_err();
    }

    #[test]
    fn test_scoped_join() {
        // create temporary directory to emulate container rootfs with symlink
        let rootfs_dir = tempdir().expect("failed to create tmpdir");
        let rootfs_path = &rootfs_dir.path();

        assert_eq!(
            scoped_join(&rootfs_path, "a").unwrap(),
            rootfs_path.join("a")
        );
        assert_eq!(
            scoped_join(&rootfs_path, "./a").unwrap(),
            rootfs_path.join("a")
        );
        assert_eq!(
            scoped_join(&rootfs_path, "././a").unwrap(),
            rootfs_path.join("a")
        );
        assert_eq!(
            scoped_join(&rootfs_path, "c/d/../../a").unwrap(),
            rootfs_path.join("a")
        );
        assert_eq!(
            scoped_join(&rootfs_path, "c/d/../../../.././a").unwrap(),
            rootfs_path.join("a")
        );
        assert_eq!(
            scoped_join(&rootfs_path, "../../a").unwrap(),
            rootfs_path.join("a")
        );
        assert_eq!(
            scoped_join(&rootfs_path, "./../a").unwrap(),
            rootfs_path.join("a")
        );
    }

    #[test]
    fn test_scoped_join_symlink() {
        // create temporary directory to emulate container rootfs with symlink
        let rootfs_dir = tempdir().expect("failed to create tmpdir");
        let rootfs_path = &rootfs_dir.path();
        DirBuilder::new()
            .recursive(true)
            .create(rootfs_dir.path().join("b/c"))
            .unwrap();
        fs::symlink("b/c", rootfs_dir.path().join("a")).unwrap();

        let target = rootfs_path.join("b/c");
        assert_eq!(scoped_join(&rootfs_path, "a").unwrap(), target);
        assert_eq!(scoped_join(&rootfs_path, "./a").unwrap(), target);
        assert_eq!(scoped_join(&rootfs_path, "././a").unwrap(), target);
        assert_eq!(scoped_join(&rootfs_path, "b/c/../../a").unwrap(), target);
        assert_eq!(
            scoped_join(&rootfs_path, "b/c/../../../.././a").unwrap(),
            target
        );
        assert_eq!(scoped_join(&rootfs_path, "../../a").unwrap(), target);
        assert_eq!(scoped_join(&rootfs_path, "./../a").unwrap(), target);
        assert_eq!(scoped_join(&rootfs_path, "a/../../../a").unwrap(), target);
        assert_eq!(scoped_join(&rootfs_path, "a/../../../b/c").unwrap(), target);
    }

    #[test]
    fn test_scoped_join_symlink_loop() {
        // create temporary directory to emulate container rootfs with symlink
        let rootfs_dir = tempdir().expect("failed to create tmpdir");
        let rootfs_path = &rootfs_dir.path();
        fs::symlink("/endpoint_b", rootfs_path.join("endpoint_a")).unwrap();
        fs::symlink("/endpoint_a", rootfs_path.join("endpoint_b")).unwrap();
        scoped_join(rootfs_path, "endpoint_a").unwrap_err();
    }

    #[test]
    fn test_scoped_join_unicode_character() {
        // create temporary directory to emulate container rootfs with symlink
        let rootfs_dir = tempdir().expect("failed to create tmpdir");
        let rootfs_path = &rootfs_dir.path().canonicalize().unwrap();

        let path = scoped_join(rootfs_path, "您好").unwrap();
        assert_eq!(path, rootfs_path.join("您好"));

        let path = scoped_join(rootfs_path, "../../../您好").unwrap();
        assert_eq!(path, rootfs_path.join("您好"));

        let path = scoped_join(rootfs_path, "。。/您好").unwrap();
        assert_eq!(path, rootfs_path.join("。。/您好"));

        let path = scoped_join(rootfs_path, "您好/../../test").unwrap();
        assert_eq!(path, rootfs_path.join("test"));
    }
}
