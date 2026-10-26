use crate::fuse;
use std::io;
use std::os::unix::io::RawFd;
use vhost::vhost_user::message::{
    VhostUserFSBackendMsg, VhostUserFSBackendMsgFlags, VHOST_USER_FS_BACKEND_ENTRIES,
};
use vhost::vhost_user::{Backend, VhostUserFrontendReqHandler};

/// Trait for virtio-fs cache requests operations.  This is mainly used to hide
/// vhost-user details from virtio-fs's fuse part.
pub trait FsCacheReqHandler: Send + Sync + 'static {
    /// Setup a dedicated mapping so that guest can access file data in DAX style.
    fn map(
        &mut self,
        foffset: u64,
        moffset: u64,
        len: u64,
        flags: u64,
        fd: RawFd,
    ) -> io::Result<()>;

    /// Remove those mappings that provide the access to file data.
    fn unmap(&mut self, requests: Vec<fuse::RemovemappingOne>) -> io::Result<()>;
}

impl FsCacheReqHandler for Backend {
    fn map(
        &mut self,
        foffset: u64,
        moffset: u64,
        len: u64,
        flags: u64,
        fd: RawFd,
    ) -> io::Result<()> {
        let mut msg: VhostUserFSBackendMsg = Default::default();
        msg.fd_offset[0] = foffset;
        msg.cache_offset[0] = moffset;
        msg.len[0] = len;
        msg.flags[0] = if (flags & fuse::SetupmappingFlags::WRITE.bits()) != 0 {
            VhostUserFSBackendMsgFlags::MAP_W | VhostUserFSBackendMsgFlags::MAP_R
        } else {
            VhostUserFSBackendMsgFlags::MAP_R
        };

        self.fs_backend_map(&msg, &fd)?;
        Ok(())
    }

    fn unmap(&mut self, requests: Vec<fuse::RemovemappingOne>) -> io::Result<()> {
        for chunk in requests.chunks(VHOST_USER_FS_BACKEND_ENTRIES) {
            let mut msg: VhostUserFSBackendMsg = Default::default();

            for (ind, req) in chunk.iter().enumerate() {
                msg.len[ind] = req.len;
                msg.cache_offset[ind] = req.moffset;
            }

            self.fs_backend_unmap(&msg)?;
        }
        Ok(())
    }
}
