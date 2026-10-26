// Copyright 2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0
//
// Portions Copyright 2017 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the THIRD-PARTY file.

use super::{
    create_inet_socket, create_sockaddr, create_unix_socket, vnet_hdr_len, Error as NetUtilError,
    MacAddr,
};
use crate::mac::MAC_ADDR_LEN;
use serde::{Deserialize, Serialize};
use std::env;
use std::fs::File;
use std::io::{Error as IoError, Read, Result as IoResult, Write};
use std::net;
use std::os::raw::*;
use std::os::unix::io::{AsRawFd, FromRawFd, RawFd};
use std::os::unix::net::UnixStream;
use std::time::Duration;
use thiserror::Error;
use vmm_sys_util::ioctl::{ioctl_with_mut_ref, ioctl_with_ref, ioctl_with_val};
use vmm_sys_util::sock_ctrl_msg::ScmSocket;

#[derive(Error, Debug)]
pub enum Error {
    #[error("Couldn't open /dev/net/tun: {0}")]
    OpenTun(IoError),
    #[error("Unable to configure tap interface: {0}")]
    ConfigureTap(IoError),
    #[error("Unable to retrieve features: {0}")]
    GetFeatures(IoError),
    #[error("Missing multiqueue support in the kernel")]
    MultiQueueKernelSupport,
    #[error("ioctl failed: {0}")]
    IoctlError(IoError),
    #[error("Failed to create a socket: {0}")]
    NetUtil(NetUtilError),
    #[error("Invalid interface name")]
    InvalidIfname,
    #[error("Error parsing MAC data: {0}")]
    MacParsing(IoError),
    #[error("Failed to connect to tap pool: {0}")]
    OpenTunConnect(IoError),
    #[error("Failed to sendmsg to tap pool: {0}")]
    OpenTunJson(serde_json::Error),
    #[error("Failed to sendmsg to tap pool: {0}")]
    OpenTunSendmsg(IoError),
    #[error("Failed to recvmsg from tap pool: {0}")]
    OpenTunRecvmsg(vmm_sys_util::errno::Error),
    #[error("Couldn't got FD from tap pool")]
    OpenTunNoFD,
    #[error("Couldn't dup FD from tap pool")]
    OpenTunDupErr(IoError),
    #[error("Couldn't set FD flags ")]
    OpenTunSetflErr(IoError),
}

pub type Result<T> = ::std::result::Result<T, Error>;

/// Handle for a network tap interface.
///
/// For now, this simply wraps the file descriptor for the tap device so methods
/// can run ioctls on the interface. The tap interface fd will be closed when
/// Tap goes out of scope, and the kernel will clean up the interface
/// automatically.
#[derive(Debug)]
pub struct Tap {
    tap_file: File,
    if_name: Vec<u8>,
    donated: bool,
}

#[cfg(test)]
impl Tap {
    pub(crate) fn from_file_for_test(tap_file: File) -> Self {
        Self {
            tap_file,
            if_name: Vec::new(),
            donated: true,
        }
    }
}

impl PartialEq for Tap {
    fn eq(&self, other: &Tap) -> bool {
        self.if_name == other.if_name
    }
}

impl std::clone::Clone for Tap {
    fn clone(&self) -> Self {
        Tap {
            tap_file: self.tap_file.try_clone().unwrap(),
            if_name: self.if_name.clone(),
            donated: self.donated,
        }
    }
}

// Returns a byte vector representing the contents of a null terminated C string which
// contains if_name.
fn build_terminated_if_name(if_name: &str) -> Result<Vec<u8>> {
    // Convert the string slice to bytes, and shadow the variable,
    // since we no longer need the &str version.
    let if_name = if_name.as_bytes();

    // TODO: the 16usize limit of the if_name member from struct Tap is pretty arbitrary.
    // We leave it as is for now, but this should be refactored at some point.
    if if_name.len() > 15 {
        return Err(Error::InvalidIfname);
    }

    let mut terminated_if_name = vec![b'\0'; if_name.len() + 1];
    terminated_if_name[..if_name.len()].copy_from_slice(if_name);

    Ok(terminated_if_name)
}

#[allow(non_snake_case)]
#[derive(Default, Deserialize, Serialize)]
struct TapRequest {
    #[serde(default)]
    name: String,
    #[serde(default)]
    sandboxId: String,
}

fn request_file_from_pool<T: Read + Write + ScmSocket>(
    sock: &mut T,
    if_name: &str,
    sandbox_id: String,
) -> Result<File> {
    let buf = &mut vec![0; 256];
    let request = TapRequest {
        name: if_name.to_string(),
        sandboxId: sandbox_id,
    };
    let start = std::time::Instant::now();

    // Send message
    let body = serde_json::to_vec(&request).map_err(Error::OpenTunJson)?;
    sock.write_all(&body).map_err(Error::OpenTunSendmsg)?;
    let ts_send = std::time::Instant::now();

    let mut fds = [0; 1];
    let mut iovecs = [libc::iovec {
        iov_base: buf.as_mut_ptr() as *mut libc::c_void,
        iov_len: buf.len(),
    }];

    let mut cnt = 0;
    let fd_count = loop {
        unsafe {
            match sock.recv_with_fds(&mut iovecs, &mut fds) {
                Ok((_, fd_count)) => break fd_count,
                Err(ref err) if err.errno() == libc::EAGAIN => {
                    std::thread::sleep(Duration::from_millis(1));
                    cnt += 1;
                    continue;
                }
                Err(e) => {
                    return Err(Error::OpenTunRecvmsg(e));
                }
            }
        };
    };
    if fd_count == 0 {
        return Err(Error::OpenTunNoFD);
    }
    let ts_recv = std::time::Instant::now();

    // Dup FD.
    let fd = unsafe { libc::dup(fds[0]) };
    if fd < 0 {
        return Err(Error::OpenTunDupErr(IoError::last_os_error()));
    }
    let ts_dup = std::time::Instant::now();

    // SAFETY: FFI call to close the received fd from socket
    unsafe { libc::close(fds[0]) };

    // Set nonblock.
    let ret = unsafe {
        let mut flags = libc::fcntl(fd, libc::F_GETFL);
        flags |= libc::O_NONBLOCK;
        libc::fcntl(fd, libc::F_SETFL, flags)
    };
    if ret < 0 {
        unsafe { libc::close(fd) };
        return Err(Error::OpenTunSetflErr(IoError::last_os_error()));
    }
    let ts_fcntl = std::time::Instant::now();

    info!(
        "Fast request tap recv done, got {} fd after {} try, send {} recv {} dup {} fcntl {}",
        fd_count,
        cnt,
        ts_send.duration_since(start).as_millis(),
        ts_recv.duration_since(ts_send).as_millis(),
        ts_dup.duration_since(ts_recv).as_millis(),
        ts_fcntl.duration_since(ts_dup).as_millis()
    );

    Ok(unsafe { File::from_raw_fd(fd) })
}

fn request_from_pool(if_name: &str, sandbox_id: String) -> Result<File> {
    let path = match env::var("CUBE_TAP_SERV_PATH") {
        Ok(var) => var,
        Err(_) => "/data/cubelet/cubetap.sock".to_string(),
    };

    let mut sock = UnixStream::connect(path).map_err(Error::OpenTunConnect)?;

    sock.set_nonblocking(true).map_err(Error::OpenTunSetflErr)?;
    sock.set_write_timeout(Some(Duration::from_millis(1)))
        .map_err(Error::OpenTunSetflErr)?;

    request_file_from_pool(&mut sock, if_name, sandbox_id)
}

impl Tap {
    // Fast path to open TAP. Open and Initialize Tap device always
    // needs some netlink related operation, which will invoke
    // rtnl_lock contention.
    // Here we introduce a new way to avoid such costs. Create Tap
    // pool, open and initialize Tap devices before launch VM. When
    // we launch new VM, try to got Tap device FD from the Tap pool,
    // then we could directly use the FD.
    pub fn lookup_from_pool(if_name: &str, sandbox_id: String) -> Result<Tap> {
        let file = request_from_pool(if_name, sandbox_id)?;

        return Ok(Tap {
            tap_file: file,
            if_name: if_name.into(),
            donated: true,
        });
    }

    pub fn open_named(if_name: &str, num_queue_pairs: usize, flags: Option<i32>) -> Result<Tap> {
        let terminated_if_name = build_terminated_if_name(if_name)?;

        let fd = unsafe {
            // Open calls are safe because we give a constant null-terminated
            // string and verify the result.
            libc::open(
                b"/dev/net/tun\0".as_ptr() as *const c_char,
                flags.unwrap_or(libc::O_RDWR | libc::O_NONBLOCK | libc::O_CLOEXEC),
            )
        };
        if fd < 0 {
            return Err(Error::OpenTun(IoError::last_os_error()));
        }

        // We just checked that the fd is valid.
        let tuntap = unsafe { File::from_raw_fd(fd) };

        // Let's validate some features before going any further.
        // ioctl is safe since we call it with a valid tap fd and check the return
        // value.
        let mut features = 0;
        let ret = unsafe { ioctl_with_mut_ref(&tuntap, net_gen::TUNGETFEATURES(), &mut features) };
        if ret < 0 {
            return Err(Error::GetFeatures(IoError::last_os_error()));
        }

        // Check if the user parameters match the kernel support for MQ
        if (features & net_gen::IFF_MULTI_QUEUE == 0) && num_queue_pairs > 1 {
            return Err(Error::MultiQueueKernelSupport);
        }

        // This is pretty messy because of the unions used by ifreq. Since we
        // don't call as_mut on the same union field more than once, this block
        // is safe.
        let mut ifreq: net_gen::ifreq = Default::default();
        unsafe {
            let ifrn_name = ifreq.ifr_ifrn.ifrn_name.as_mut();
            let name_slice = &mut ifrn_name[..terminated_if_name.len()];
            name_slice.copy_from_slice(terminated_if_name.as_slice());
            ifreq.ifr_ifru.ifru_flags =
                (net_gen::IFF_TAP | net_gen::IFF_NO_PI | net_gen::IFF_VNET_HDR) as c_short;
            if num_queue_pairs > 1 {
                ifreq.ifr_ifru.ifru_flags |= net_gen::IFF_MULTI_QUEUE as c_short;
            }
        }

        // ioctl is safe since we call it with a valid tap fd and check the return
        // value.
        let ret = unsafe { ioctl_with_mut_ref(&tuntap, net_gen::TUNSETIFF(), &mut ifreq) };
        if ret < 0 {
            return Err(Error::ConfigureTap(IoError::last_os_error()));
        }

        let mut if_name = unsafe { ifreq.ifr_ifrn.ifrn_name }.to_vec();
        if_name.truncate(terminated_if_name.len() - 1);
        // Safe since only the name is accessed, and it's cloned out.
        Ok(Tap {
            tap_file: tuntap,
            if_name,
            donated: false,
        })
    }

    /// Create a new tap interface.
    pub fn new(num_queue_pairs: usize) -> Result<Tap> {
        Self::open_named("vmtap%d", num_queue_pairs, None)
    }

    pub fn from_tap_fd(fd: RawFd, num_queue_pairs: usize) -> Result<Tap> {
        // Ensure that the file is opened non-blocking, this is particularly
        // needed when opened via the shell for macvtap.
        let ret = unsafe {
            let mut flags = libc::fcntl(fd, libc::F_GETFL);
            flags |= libc::O_NONBLOCK;
            libc::fcntl(fd, libc::F_SETFL, flags)
        };
        if ret < 0 {
            return Err(Error::ConfigureTap(IoError::last_os_error()));
        }

        let tap_file = unsafe { File::from_raw_fd(fd) };
        let mut ifreq: net_gen::ifreq = Default::default();

        // Get current config including name
        let ret = unsafe { ioctl_with_mut_ref(&tap_file, net_gen::TUNGETIFF(), &mut ifreq) };
        if ret < 0 {
            return Err(Error::IoctlError(IoError::last_os_error()));
        }
        // We only access one field of the ifru union, hence this is safe.
        let if_name = unsafe { ifreq.ifr_ifrn.ifrn_name }.to_vec();

        // Try and update flags. Depending on how the tap was created (macvtap
        // or via open_named()) this might return -EEXIST so we just ignore that.
        unsafe {
            ifreq.ifr_ifru.ifru_flags =
                (net_gen::IFF_TAP | net_gen::IFF_NO_PI | net_gen::IFF_VNET_HDR) as c_short;
            if num_queue_pairs > 1 {
                ifreq.ifr_ifru.ifru_flags |= net_gen::IFF_MULTI_QUEUE as c_short;
            }
        }
        let ret = unsafe { ioctl_with_mut_ref(&tap_file, net_gen::TUNSETIFF(), &mut ifreq) };
        if ret < 0 && IoError::last_os_error().raw_os_error().unwrap() != libc::EEXIST {
            return Err(Error::ConfigureTap(IoError::last_os_error()));
        }

        let tap = Tap {
            tap_file,
            if_name,
            donated: false,
        };
        let vnet_hdr_size = vnet_hdr_len() as i32;
        tap.set_vnet_hdr_size(vnet_hdr_size)?;

        Ok(tap)
    }

    /// Set the host-side IP address for the tap interface.
    pub fn set_ip_addr(&self, ip_addr: net::Ipv4Addr) -> Result<()> {
        let sock = create_inet_socket().map_err(Error::NetUtil)?;
        let addr = create_sockaddr(ip_addr);

        let mut ifreq = self.get_ifreq();

        ifreq.ifr_ifru.ifru_addr = addr;

        // ioctl is safe. Called with a valid sock fd, and we check the return.
        let ret =
            unsafe { ioctl_with_ref(&sock, net_gen::sockios::SIOCSIFADDR as c_ulong, &ifreq) };
        if ret < 0 {
            return Err(Error::IoctlError(IoError::last_os_error()));
        }

        Ok(())
    }

    /// Set mac addr for tap interface.
    pub fn set_mac_addr(&self, addr: MacAddr) -> Result<()> {
        // Checking if the mac address already matches the desired one
        // is useful to avoid making the "set ioctl" in the case where
        // the VMM is running without the privilege to do that.
        // In practice this comes from a reboot after the configuration
        // has been update with the kernel generated address.
        if self.get_mac_addr()? == addr {
            return Ok(());
        }

        let sock = create_unix_socket().map_err(Error::NetUtil)?;

        let mut ifreq = self.get_ifreq();

        // ioctl is safe. Called with a valid sock fd, and we check the return.
        let ret =
            unsafe { ioctl_with_ref(&sock, net_gen::sockios::SIOCGIFHWADDR as c_ulong, &ifreq) };
        if ret < 0 {
            return Err(Error::IoctlError(IoError::last_os_error()));
        }
        // We only access one field of the ifru union, hence this is safe.
        unsafe {
            let ifru_hwaddr = &mut ifreq.ifr_ifru.ifru_hwaddr;
            for (i, v) in addr.get_bytes().iter().enumerate() {
                ifru_hwaddr.sa_data[i] = *v as c_uchar;
            }
        }

        // ioctl is safe. Called with a valid sock fd, and we check the return.
        let ret =
            unsafe { ioctl_with_ref(&sock, net_gen::sockios::SIOCSIFHWADDR as c_ulong, &ifreq) };
        if ret < 0 {
            return Err(Error::IoctlError(IoError::last_os_error()));
        }

        Ok(())
    }

    /// Get mac addr for tap interface.
    pub fn get_mac_addr(&self) -> Result<MacAddr> {
        let sock = create_unix_socket().map_err(Error::NetUtil)?;

        let ifreq = self.get_ifreq();

        // ioctl is safe. Called with a valid sock fd, and we check the return.
        let ret =
            unsafe { ioctl_with_ref(&sock, net_gen::sockios::SIOCGIFHWADDR as c_ulong, &ifreq) };
        if ret < 0 {
            return Err(Error::IoctlError(IoError::last_os_error()));
        }

        // We only access one field of the ifru union, hence this is safe.
        let addr = unsafe {
            MacAddr::from_bytes(&ifreq.ifr_ifru.ifru_hwaddr.sa_data[0..MAC_ADDR_LEN])
                .map_err(Error::MacParsing)?
        };
        Ok(addr)
    }

    /// Set the netmask for the subnet that the tap interface will exist on.
    pub fn set_netmask(&self, netmask: net::Ipv4Addr) -> Result<()> {
        let sock = create_inet_socket().map_err(Error::NetUtil)?;
        let addr = create_sockaddr(netmask);

        let mut ifreq = self.get_ifreq();

        ifreq.ifr_ifru.ifru_addr = addr;

        // ioctl is safe. Called with a valid sock fd, and we check the return.
        let ret =
            unsafe { ioctl_with_ref(&sock, net_gen::sockios::SIOCSIFNETMASK as c_ulong, &ifreq) };
        if ret < 0 {
            return Err(Error::IoctlError(IoError::last_os_error()));
        }

        Ok(())
    }

    pub fn mtu(&self) -> Result<i32> {
        let sock = create_unix_socket().map_err(Error::NetUtil)?;

        let ifreq = self.get_ifreq();

        // ioctl is safe. Called with a valid sock fd, and we check the return.
        let ret = unsafe { ioctl_with_ref(&sock, net_gen::sockios::SIOCGIFMTU as c_ulong, &ifreq) };
        if ret < 0 {
            return Err(Error::IoctlError(IoError::last_os_error()));
        }

        let mtu = unsafe { ifreq.ifr_ifru.ifru_mtu };

        Ok(mtu)
    }

    pub fn set_mtu(&self, mtu: i32) -> Result<()> {
        let sock = create_unix_socket().map_err(Error::NetUtil)?;

        let mut ifreq = self.get_ifreq();
        ifreq.ifr_ifru.ifru_mtu = mtu;

        // ioctl is safe. Called with a valid sock fd, and we check the return.
        let ret = unsafe { ioctl_with_ref(&sock, net_gen::sockios::SIOCSIFMTU as c_ulong, &ifreq) };
        if ret < 0 {
            return Err(Error::IoctlError(IoError::last_os_error()));
        }

        Ok(())
    }

    /// Set the offload flags for the tap interface.
    pub fn set_offload(&self, flags: c_uint) -> Result<()> {
        // ioctl is safe. Called with a valid tap fd, and we check the return.
        let ret =
            unsafe { ioctl_with_val(&self.tap_file, net_gen::TUNSETOFFLOAD(), flags as c_ulong) };
        if ret < 0 {
            return Err(Error::IoctlError(IoError::last_os_error()));
        }

        Ok(())
    }

    /// Enable the tap interface.
    pub fn enable(&self) -> Result<()> {
        let sock = create_unix_socket().map_err(Error::NetUtil)?;

        let mut ifreq = self.get_ifreq();

        let ret =
            unsafe { ioctl_with_ref(&sock, net_gen::sockios::SIOCGIFFLAGS as c_ulong, &ifreq) };
        if ret < 0 {
            return Err(Error::IoctlError(IoError::last_os_error()));
        }

        // If TAP device is already up don't try and enable it
        let ifru_flags = unsafe { ifreq.ifr_ifru.ifru_flags };
        if ifru_flags & net_gen::net_device_flags_IFF_UP as i16
            == net_gen::net_device_flags_IFF_UP as i16
        {
            return Ok(());
        }

        ifreq.ifr_ifru.ifru_flags = net_gen::net_device_flags_IFF_UP as i16;

        // ioctl is safe. Called with a valid sock fd, and we check the return.
        let ret =
            unsafe { ioctl_with_ref(&sock, net_gen::sockios::SIOCSIFFLAGS as c_ulong, &ifreq) };
        if ret < 0 {
            return Err(Error::IoctlError(IoError::last_os_error()));
        }

        Ok(())
    }

    /// Set the size of the vnet hdr.
    pub fn set_vnet_hdr_size(&self, size: c_int) -> Result<()> {
        // ioctl is safe. Called with a valid tap fd, and we check the return.
        let ret = unsafe { ioctl_with_ref(&self.tap_file, net_gen::TUNSETVNETHDRSZ(), &size) };
        if ret < 0 {
            return Err(Error::IoctlError(IoError::last_os_error()));
        }

        Ok(())
    }

    fn get_ifreq(&self) -> net_gen::ifreq {
        let mut ifreq: net_gen::ifreq = Default::default();

        // This sets the name of the interface, which is the only entry
        // in a single-field union.
        unsafe {
            let ifrn_name = ifreq.ifr_ifrn.ifrn_name.as_mut();
            let name_slice = &mut ifrn_name[..self.if_name.len()];
            name_slice.copy_from_slice(&self.if_name);
        }

        ifreq
    }

    pub fn get_if_name(&self) -> Vec<u8> {
        self.if_name.clone()
    }

    pub fn is_donated(&self) -> bool {
        return self.donated;
    }
}

impl Read for Tap {
    fn read(&mut self, buf: &mut [u8]) -> IoResult<usize> {
        self.tap_file.read(buf)
    }
}

impl Write for Tap {
    fn write(&mut self, buf: &[u8]) -> IoResult<usize> {
        self.tap_file.write(buf)
    }

    fn flush(&mut self) -> IoResult<()> {
        Ok(())
    }
}

impl AsRawFd for Tap {
    fn as_raw_fd(&self) -> RawFd {
        self.tap_file.as_raw_fd()
    }
}

#[cfg(test)]
mod tests {
    use std::net::Ipv4Addr;
    use std::str;
    use std::sync::{mpsc, Mutex};
    use std::thread;
    use std::time::Duration;

    use once_cell::sync::Lazy;

    use pnet::packet::ethernet::{EtherTypes, EthernetPacket, MutableEthernetPacket};
    use pnet::packet::ip::IpNextHeaderProtocols;
    use pnet::packet::ipv4::{Ipv4Packet, MutableIpv4Packet};
    use pnet::packet::udp::{MutableUdpPacket, UdpPacket};
    use pnet::packet::{MutablePacket, Packet};
    use pnet::util::MacAddr;
    use pnet_datalink::Channel::Ethernet;
    use pnet_datalink::{DataLinkReceiver, DataLinkSender, NetworkInterface};

    use super::*;

    static DATA_STRING: &str = "test for tap";
    static SUBNET_MASK: &str = "255.255.255.0";

    // We needed to have a mutex as a global variable, so we used once_cell for testing. The main
    // potential problem, caused by tests being run in parallel by cargo, is creating different
    // TAPs and trying to associate the same address, so we hide the IP address &str behind this
    // mutex, more as a convention to remember to lock it at the very beginning of each function
    // susceptible to this issue. Another variant is to use a different IP address per function,
    // but we must remember to pick an unique one each time.
    static TAP_IP_LOCK: Lazy<Mutex<&'static str>> = Lazy::new(|| Mutex::new("192.168.241.1"));

    // Describes the outcomes we are currently interested in when parsing a packet (we use
    // an UDP packet for testing).
    struct ParsedPkt<'a> {
        eth: EthernetPacket<'a>,
        ipv4: Option<Ipv4Packet<'a>>,
        udp: Option<UdpPacket<'a>>,
    }

    impl<'a> ParsedPkt<'a> {
        fn new(buf: &'a [u8]) -> Self {
            let eth = EthernetPacket::new(buf).unwrap();
            let mut ipv4 = None;
            let mut udp = None;

            if eth.get_ethertype() == EtherTypes::Ipv4 {
                let ipv4_start = 14;
                ipv4 = Some(Ipv4Packet::new(&buf[ipv4_start..]).unwrap());

                // Hiding the old ipv4 variable for the rest of this block.
                let ipv4 = Ipv4Packet::new(eth.payload()).unwrap();

                if ipv4.get_next_level_protocol() == IpNextHeaderProtocols::Udp {
                    // The value in header_length indicates the number of 32 bit words
                    // that make up the header, not the actual length in bytes.
                    let udp_start = ipv4_start + ipv4.get_header_length() as usize * 4;
                    udp = Some(UdpPacket::new(&buf[udp_start..]).unwrap());
                }
            }

            ParsedPkt { eth, ipv4, udp }
        }

        fn print(&self) {
            print!(
                "{} {} {} ",
                self.eth.get_source(),
                self.eth.get_destination(),
                self.eth.get_ethertype()
            );
            if let Some(ref ipv4) = self.ipv4 {
                print!(
                    "{} {} {} ",
                    ipv4.get_source(),
                    ipv4.get_destination(),
                    ipv4.get_next_level_protocol()
                );
            }
            if let Some(ref udp) = self.udp {
                print!(
                    "{} {} {}",
                    udp.get_source(),
                    udp.get_destination(),
                    str::from_utf8(udp.payload()).unwrap()
                );
            }
            println!();
        }
    }

    fn tap_name_to_string(tap: &Tap) -> String {
        let null_pos = tap.if_name.iter().position(|x| *x == 0).unwrap();
        str::from_utf8(&tap.if_name[..null_pos])
            .unwrap()
            .to_string()
    }

    // Given a buffer of appropriate size, this fills in the relevant fields based on the
    // provided information. Payload refers to the UDP payload.
    fn pnet_build_packet(buf: &mut [u8], dst_mac: MacAddr, payload: &[u8]) {
        let mut eth = MutableEthernetPacket::new(buf).unwrap();
        eth.set_source(MacAddr::new(0x06, 0, 0, 0, 0, 0));
        eth.set_destination(dst_mac);
        eth.set_ethertype(EtherTypes::Ipv4);

        let mut ipv4 = MutableIpv4Packet::new(eth.payload_mut()).unwrap();
        ipv4.set_version(4);
        ipv4.set_header_length(5);
        ipv4.set_total_length(20 + 8 + payload.len() as u16);
        ipv4.set_ttl(200);
        ipv4.set_next_level_protocol(IpNextHeaderProtocols::Udp);
        ipv4.set_source(Ipv4Addr::new(192, 168, 241, 1));
        ipv4.set_destination(Ipv4Addr::new(192, 168, 241, 2));

        let mut udp = MutableUdpPacket::new(ipv4.payload_mut()).unwrap();
        udp.set_source(1000);
        udp.set_destination(1001);
        udp.set_length(8 + payload.len() as u16);
        udp.set_payload(payload);
    }

    // Sends a test packet on the interface named "ifname".
    fn pnet_send_packet(ifname: String) {
        let payload = DATA_STRING.as_bytes();

        // eth hdr + ip hdr + udp hdr + payload len
        let buf_size = 14 + 20 + 8 + payload.len();

        let (mac, mut tx, _) = pnet_get_mac_tx_rx(ifname);

        let res = tx.build_and_send(1, buf_size, &mut |buf| {
            pnet_build_packet(buf, mac, payload);
        });
        // Make sure build_and_send() -> Option<io::Result<()>> succeeds.
        res.unwrap().unwrap();
    }

    // For a given interface name, this returns a tuple that contains the MAC address of the
    // interface, an object that can be used to send Ethernet frames, and a receiver of
    // Ethernet frames arriving at the specified interface.
    fn pnet_get_mac_tx_rx(
        ifname: String,
    ) -> (MacAddr, Box<dyn DataLinkSender>, Box<dyn DataLinkReceiver>) {
        let interface_name_matches = |iface: &NetworkInterface| iface.name == ifname;

        // Find the network interface with the provided name.
        let interfaces = pnet_datalink::interfaces();
        let interface = interfaces.into_iter().find(interface_name_matches).unwrap();

        if let Ok(Ethernet(tx, rx)) = pnet_datalink::channel(&interface, Default::default()) {
            (interface.mac.unwrap(), tx, rx)
        } else {
            panic!("datalink channel error or unhandled channel type");
        }
    }

    #[test]
    fn test_tap_create() {
        let _tap_ip_guard = TAP_IP_LOCK.lock().unwrap();

        let t = Tap::new(1).unwrap();
        println!("created tap: {:?}", t);
    }

    #[test]
    fn test_tap_from_fd() {
        let _tap_ip_guard = TAP_IP_LOCK.lock().unwrap();

        let orig_tap = Tap::new(1).unwrap();
        let fd = orig_tap.as_raw_fd();
        let _new_tap = Tap::from_tap_fd(fd, 1).unwrap();
    }

    #[test]
    fn test_tap_configure() {
        // This should be the first thing to be called inside the function, so everything else
        // is torn down by the time the mutex is automatically released. Also, we should
        // explicitly bind the MutexGuard to a variable via let, the make sure it lives until
        // the end of the function.
        let tap_ip_guard = TAP_IP_LOCK.lock().unwrap();

        let tap = Tap::new(1).unwrap();
        let ip_addr: net::Ipv4Addr = (*tap_ip_guard).parse().unwrap();
        let netmask: net::Ipv4Addr = SUBNET_MASK.parse().unwrap();

        let ret = tap.set_ip_addr(ip_addr);
        assert!(ret.is_ok());
        let ret = tap.set_netmask(netmask);
        assert!(ret.is_ok());
    }

    #[test]
    fn test_set_options() {
        let _tap_ip_guard = TAP_IP_LOCK.lock().unwrap();

        // This line will fail to provide an initialized FD if the test is not run as root.
        let tap = Tap::new(1).unwrap();
        tap.set_vnet_hdr_size(16).unwrap();
        tap.set_offload(0).unwrap();
    }

    #[test]
    fn test_tap_enable() {
        let _tap_ip_guard = TAP_IP_LOCK.lock().unwrap();

        let tap = Tap::new(1).unwrap();
        let ret = tap.enable();
        assert!(ret.is_ok());
    }

    #[test]
    fn test_raw_fd() {
        let _tap_ip_guard = TAP_IP_LOCK.lock().unwrap();

        let tap = Tap::new(1).unwrap();
        assert_eq!(tap.as_raw_fd(), tap.tap_file.as_raw_fd());
    }

    #[test]
    fn test_read() {
        let tap_ip_guard = TAP_IP_LOCK.lock().unwrap();

        let mut tap = Tap::new(1).unwrap();
        tap.set_ip_addr((*tap_ip_guard).parse().unwrap()).unwrap();
        tap.set_netmask(SUBNET_MASK.parse().unwrap()).unwrap();
        tap.enable().unwrap();

        // Send a packet to the interface. We expect to be able to receive it on the associated fd.
        pnet_send_packet(tap_name_to_string(&tap));

        let mut buf = [0u8; 4096];

        let mut found_packet_sz = None;

        // In theory, this could actually loop forever if something keeps sending data through the
        // tap interface, but it's highly unlikely.
        while found_packet_sz.is_none() {
            let result = tap.read(&mut buf);
            assert!(result.is_ok());

            let size = result.unwrap();

            // We skip the first 10 bytes because the IFF_VNET_HDR flag is set when the interface
            // is created, and the legacy header is 10 bytes long without a certain flag which
            // is not set in Tap::new().
            let eth_bytes = &buf[10..size];

            let packet = EthernetPacket::new(eth_bytes).unwrap();
            if packet.get_ethertype() != EtherTypes::Ipv4 {
                // not an IPv4 packet
                continue;
            }

            let ipv4_bytes = &eth_bytes[14..];
            let packet = Ipv4Packet::new(ipv4_bytes).unwrap();

            // Our packet should carry an UDP payload, and not contain IP options.
            if packet.get_next_level_protocol() != IpNextHeaderProtocols::Udp
                && packet.get_header_length() != 5
            {
                continue;
            }

            let udp_bytes = &ipv4_bytes[20..];

            let udp_len = UdpPacket::new(udp_bytes).unwrap().get_length() as usize;

            // Skip the header bytes.
            let inner_string = str::from_utf8(&udp_bytes[8..udp_len]).unwrap();

            if inner_string.eq(DATA_STRING) {
                found_packet_sz = Some(size);
                break;
            }
        }

        assert!(found_packet_sz.is_some());
    }

    #[test]
    fn test_write() {
        let tap_ip_guard = TAP_IP_LOCK.lock().unwrap();

        let mut tap = Tap::new(1).unwrap();
        tap.set_ip_addr((*tap_ip_guard).parse().unwrap()).unwrap();
        tap.set_netmask(SUBNET_MASK.parse().unwrap()).unwrap();
        tap.enable().unwrap();

        let (mac, _, mut rx) = pnet_get_mac_tx_rx(tap_name_to_string(&tap));

        let payload = DATA_STRING.as_bytes();

        // vnet hdr + eth hdr + ip hdr + udp hdr + payload len
        let buf_size = 10 + 14 + 20 + 8 + payload.len();

        let mut buf = vec![0u8; buf_size];
        // leave the vnet hdr as is
        pnet_build_packet(&mut buf[10..], mac, payload);

        assert!(tap.write(&buf[..]).is_ok());
        assert!(tap.flush().is_ok());

        let (channel_tx, channel_rx) = mpsc::channel();

        // We use a separate thread to wait for the test packet because the API exposed by pnet is
        // blocking. This thread will be killed when the main thread exits.
        let _handle = thread::spawn(move || loop {
            let buf = rx.next().unwrap();
            let p = ParsedPkt::new(buf);
            p.print();

            if let Some(ref udp) = p.udp {
                if payload == udp.payload() {
                    channel_tx.send(true).unwrap();
                    break;
                }
            }
        });

        // We wait for at most SLEEP_MILLIS * SLEEP_ITERS milliseconds for the reception of the
        // test packet to be detected.
        static SLEEP_MILLIS: u64 = 500;
        static SLEEP_ITERS: u32 = 6;

        let mut found_test_packet = false;

        for _ in 0..SLEEP_ITERS {
            thread::sleep(Duration::from_millis(SLEEP_MILLIS));
            if let Ok(true) = channel_rx.try_recv() {
                found_test_packet = true;
                break;
            }
        }

        assert!(found_test_packet);
    }
}
