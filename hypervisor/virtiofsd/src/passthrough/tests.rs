// SPDX-License-Identifier: Apache-2.0

use super::*;
use std::fs;
use std::io::Read;
use std::os::unix::ffi::OsStrExt;
use std::path::PathBuf;
use std::sync::{Mutex, MutexGuard, OnceLock};
use std::time::{SystemTime, UNIX_EPOCH};

/// Return the process-wide lock that serializes tests using the proc-fd xattr
/// path.  The daemon workers call unshare(CLONE_FS) so each thread has its own
/// CWD and umask; tests hold this lock instead because unshare may be blocked
/// by CI container seccomp policies.
fn cwd_lock() -> &'static Mutex<()> {
    static LOCK: OnceLock<Mutex<()>> = OnceLock::new();
    LOCK.get_or_init(|| Mutex::new(()))
}

struct Fixture {
    fs: PassthroughFs,
    path: PathBuf,
    ctx: Context,
    saved_umask: libc::mode_t,
    _cwd_lock: MutexGuard<'static, ()>,
}

impl Fixture {
    fn new(xattr: bool) -> Self {
        // Hold the serialization lock for the Fixture's lifetime so that no two
        // xattr tests run concurrently and race on the process CWD or umask.
        // unwrap_or_else recovers from lock poisoning caused by a previous panic
        // so that one failing test does not cascade into all subsequent ones.
        let _cwd_lock = cwd_lock()
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());

        // Save the current umask before PassthroughFs::new calls umask(0).
        // Restored in Drop so the change does not leak to unrelated tests.
        // Safe because umask does not access userspace memory.
        let saved_umask = unsafe { libc::umask(0o022) };

        static NEXT: AtomicU64 = AtomicU64::new(0);
        let path = std::env::temp_dir().join(format!(
            "virtiofsd-xattr-{}-{}-{}",
            std::process::id(),
            SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap()
                .as_nanos(),
            NEXT.fetch_add(1, Ordering::Relaxed)
        ));
        fs::create_dir(&path).unwrap();
        let fs = PassthroughFs::new(Config {
            root_dir: path.to_str().unwrap().to_owned(),
            xattr,
            ..Default::default()
        })
        .unwrap();
        fs.open_root_node().unwrap();
        // Safe because these calls do not access userspace memory.
        let ctx = unsafe {
            Context {
                uid: libc::getuid(),
                gid: libc::getgid(),
                pid: libc::getpid(),
            }
        };
        Self {
            fs,
            path,
            ctx,
            saved_umask,
            _cwd_lock,
        }
    }

    fn inode(&self, name: &str) -> Inode {
        self.fs
            .lookup(self.ctx, fuse::ROOT_ID, &CString::new(name).unwrap(), None)
            .unwrap()
            .inode
    }

    fn set_xattr(&self, inode: Inode, name: &CStr, value: &[u8]) {
        self.fs
            .setxattr(self.ctx, inode, name, value, 0, SetxattrFlags::empty())
            .unwrap();
    }
}

impl Drop for Fixture {
    fn drop(&mut self) {
        // Restore the umask that PassthroughFs::new changed so we do not leave
        // umask(0) in place for unrelated tests that run after this one.
        // Safe because umask does not access userspace memory.
        unsafe { libc::umask(self.saved_umask) };
        fs::remove_dir_all(&self.path).unwrap();
    }
}

#[test]
fn getxattr_does_not_open_regular_file() {
    let fixture = Fixture::new(true);
    fs::write(fixture.path.join("file"), b"hello world").unwrap();
    let inode = fixture.inode("file");
    let name = CString::new("user.virtiofsd_test").unwrap();
    fixture.set_xattr(inode, &name, b"value");

    // Watch after setup: the metadata reads below must not generate data-file
    // open/close events. An extra open/close can undo O_TRUNC on s3fs (#1991).
    // Safe because inotify_init1 does not access userspace memory.
    let fd = unsafe { libc::inotify_init1(libc::IN_NONBLOCK | libc::IN_CLOEXEC) };
    assert!(fd >= 0);
    // Safe because fd is a newly allocated, owned descriptor.
    let mut watch = unsafe { File::from_raw_fd(fd) };
    let path = CString::new(fixture.path.join("file").as_os_str().as_bytes()).unwrap();
    // Safe because path is NUL-terminated and the return value is checked.
    let wd = unsafe {
        libc::inotify_add_watch(fd, path.as_ptr(), libc::IN_OPEN | libc::IN_CLOSE_NOWRITE)
    };
    assert!(wd >= 0);

    assert!(matches!(
        fixture.fs.getxattr(fixture.ctx, inode, &name, 0).unwrap(),
        GetxattrReply::Count(5)
    ));
    assert!(matches!(
        fixture.fs.getxattr(fixture.ctx, inode, &name, 5).unwrap(),
        GetxattrReply::Value(value) if value == b"value"
    ));
    let mut events = [0; 1024];
    let err = watch
        .read(&mut events)
        .expect_err("getxattr must not open or close the data file");
    assert_eq!(err.raw_os_error(), Some(libc::EAGAIN));
}

#[test]
fn getxattr_replies_for_files_and_directories() {
    let fixture = Fixture::new(true);
    fs::write(fixture.path.join("file"), b"data").unwrap();
    fs::create_dir(fixture.path.join("dir")).unwrap();
    let name = CString::new("user.virtiofsd_test").unwrap();
    let missing = CString::new("user.missing").unwrap();
    let expected = b"\0hi\xff";
    for entry in ["file", "dir"] {
        let inode = fixture.inode(entry);
        fixture.set_xattr(inode, &name, expected);
        assert!(matches!(
            fixture.fs.getxattr(fixture.ctx, inode, &name, 0).unwrap(),
            GetxattrReply::Count(4)
        ));
        assert!(matches!(
            fixture.fs.getxattr(fixture.ctx, inode, &name, 16).unwrap(),
            GetxattrReply::Value(value) if value == expected
        ));
        let err = fixture
            .fs
            .getxattr(fixture.ctx, inode, &name, 1)
            .err()
            .unwrap();
        assert_eq!(err.raw_os_error(), Some(libc::ERANGE));
        let err = fixture
            .fs
            .getxattr(fixture.ctx, inode, &missing, 0)
            .err()
            .unwrap();
        assert_eq!(err.raw_os_error(), Some(libc::ENODATA));
    }
}

#[test]
fn getxattr_uses_inode_after_unlink() {
    let fixture = Fixture::new(true);
    fs::write(fixture.path.join("file"), b"data").unwrap();
    let inode = fixture.inode("file");
    let name = CString::new("user.virtiofsd_test").unwrap();
    fixture.set_xattr(inode, &name, b"original");
    fs::remove_file(fixture.path.join("file")).unwrap();
    assert!(matches!(
        fixture.fs.getxattr(fixture.ctx, inode, &name, 8).unwrap(),
        GetxattrReply::Value(value) if value == b"original"
    ));
}

#[test]
fn getxattr_disabled_returns_enosys() {
    let fixture = Fixture::new(false);
    let name = CString::new("user.virtiofsd_test").unwrap();
    let err = fixture
        .fs
        .getxattr(fixture.ctx, fuse::ROOT_ID, &name, 0)
        .err()
        .unwrap();
    assert_eq!(err.raw_os_error(), Some(libc::ENOSYS));
}

#[test]
fn remaining_xattrs_do_not_open_regular_file() {
    let fixture = Fixture::new(true);
    let path = fixture.path.join("file");
    fs::write(&path, b"hello world").unwrap();
    let inode = fixture.inode("file");
    let name = CString::new("user.virtiofsd_test").unwrap();
    let fd = unsafe { libc::inotify_init1(libc::IN_NONBLOCK | libc::IN_CLOEXEC) };
    assert!(fd >= 0);
    let mut watch = unsafe { File::from_raw_fd(fd) };
    let path = CString::new(path.as_os_str().as_bytes()).unwrap();
    assert!(
        unsafe {
            libc::inotify_add_watch(fd, path.as_ptr(), libc::IN_OPEN | libc::IN_CLOSE_NOWRITE)
        } >= 0
    );

    fixture.set_xattr(inode, &name, b"value");
    let mut events = [0; 1024];
    assert_eq!(
        watch.read(&mut events).unwrap_err().raw_os_error(),
        Some(libc::EAGAIN),
        "setxattr must not open or close the data file"
    );
    fixture.fs.listxattr(fixture.ctx, inode, 0).unwrap();
    fixture.fs.listxattr(fixture.ctx, inode, 4096).unwrap();
    assert_eq!(
        watch.read(&mut events).unwrap_err().raw_os_error(),
        Some(libc::EAGAIN),
        "listxattr must not open or close the data file"
    );
    fixture.fs.removexattr(fixture.ctx, inode, &name).unwrap();
    assert_eq!(
        watch.read(&mut events).unwrap_err().raw_os_error(),
        Some(libc::EAGAIN),
        "removexattr must not open or close the data file"
    );
}

#[test]
fn remaining_xattr_semantics_for_files_directories_and_unlinked_inodes() {
    let fixture = Fixture::new(true);
    fs::write(fixture.path.join("file"), b"data").unwrap();
    fs::create_dir(fixture.path.join("dir")).unwrap();
    fs::write(fixture.path.join("unlinked"), b"data").unwrap();
    let unlinked = fixture.inode("unlinked");
    fs::remove_file(fixture.path.join("unlinked")).unwrap();
    let name = CString::new("user.virtiofsd_test").unwrap();
    for inode in [fixture.inode("file"), fixture.inode("dir"), unlinked] {
        fixture
            .fs
            .setxattr(
                fixture.ctx,
                inode,
                &name,
                b"first",
                libc::XATTR_CREATE as u32,
                SetxattrFlags::empty(),
            )
            .unwrap();
        assert_eq!(
            fixture
                .fs
                .setxattr(
                    fixture.ctx,
                    inode,
                    &name,
                    b"duplicate",
                    libc::XATTR_CREATE as u32,
                    SetxattrFlags::empty()
                )
                .unwrap_err()
                .raw_os_error(),
            Some(libc::EEXIST)
        );
        fixture
            .fs
            .setxattr(
                fixture.ctx,
                inode,
                &name,
                b"replacement",
                libc::XATTR_REPLACE as u32,
                SetxattrFlags::empty(),
            )
            .unwrap();
        assert!(matches!(
            fixture.fs.getxattr(fixture.ctx, inode, &name, 32).unwrap(),
            GetxattrReply::Value(value) if value == b"replacement"
        ));
        let count = match fixture.fs.listxattr(fixture.ctx, inode, 0).unwrap() {
            ListxattrReply::Count(count) => count,
            _ => panic!("expected xattr list size"),
        };
        assert!(count >= name.as_bytes_with_nul().len() as u32);
        assert!(matches!(
            fixture.fs.listxattr(fixture.ctx, inode, count).unwrap(),
            ListxattrReply::Names(names) if names.split(|byte| *byte == 0).any(|entry| entry == name.as_bytes())
        ));
        assert_eq!(
            fixture
                .fs
                .listxattr(fixture.ctx, inode, 1)
                .err()
                .unwrap()
                .raw_os_error(),
            Some(libc::ERANGE)
        );
        fixture.fs.removexattr(fixture.ctx, inode, &name).unwrap();
        assert_eq!(
            fixture
                .fs
                .removexattr(fixture.ctx, inode, &name)
                .unwrap_err()
                .raw_os_error(),
            Some(libc::ENODATA)
        );
        assert_eq!(
            fixture
                .fs
                .setxattr(
                    fixture.ctx,
                    inode,
                    &name,
                    b"missing",
                    libc::XATTR_REPLACE as u32,
                    SetxattrFlags::empty()
                )
                .unwrap_err()
                .raw_os_error(),
            Some(libc::ENODATA)
        );
    }
}

#[test]
fn remaining_xattrs_disabled_return_enosys() {
    let fixture = Fixture::new(false);
    let name = CString::new("user.virtiofsd_test").unwrap();
    assert_eq!(
        fixture
            .fs
            .setxattr(
                fixture.ctx,
                fuse::ROOT_ID,
                &name,
                b"value",
                0,
                SetxattrFlags::empty()
            )
            .unwrap_err()
            .raw_os_error(),
        Some(libc::ENOSYS)
    );
    assert_eq!(
        fixture
            .fs
            .listxattr(fixture.ctx, fuse::ROOT_ID, 0)
            .err()
            .unwrap()
            .raw_os_error(),
        Some(libc::ENOSYS)
    );
    assert_eq!(
        fixture
            .fs
            .removexattr(fixture.ctx, fuse::ROOT_ID, &name)
            .unwrap_err()
            .raw_os_error(),
        Some(libc::ENOSYS)
    );
}

#[test]
fn setxattr_clears_remapped_file_capabilities_without_reopening() {
    let mut fixture = Fixture::new(true);
    fs::write(fixture.path.join("file"), b"data").unwrap();
    let inode = fixture.inode("file");
    let capability = CString::new("user.remapped_capability").unwrap();
    let name = CString::new("user.virtiofsd_test").unwrap();
    fixture.set_xattr(inode, &capability, b"capability");
    fixture.fs.cfg.xattr_security_capability = Some(capability.clone());
    fixture.set_xattr(inode, &name, b"value");
    assert_eq!(
        fixture
            .fs
            .getxattr(fixture.ctx, inode, &capability, 0)
            .err()
            .unwrap()
            .raw_os_error(),
        Some(libc::ENODATA)
    );
    fixture.set_xattr(inode, &name, b"replacement");
    assert!(matches!(
        fixture.fs.getxattr(fixture.ctx, inode, &name, 32).unwrap(),
        GetxattrReply::Value(value) if value == b"replacement"
    ));
}
