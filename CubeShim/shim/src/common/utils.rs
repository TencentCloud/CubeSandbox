// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use std::collections::{HashMap, HashSet};
use std::fs::{self, File};
use std::future::Future;
use std::io::{ErrorKind, IoSlice, Read, Write};
use std::os::unix::fs::OpenOptionsExt;
use std::os::unix::io::AsRawFd;
use std::path::{Path, PathBuf};
use std::process;
use std::str::FromStr;
use std::time::Duration;

use cube_hypervisor::config::{RateLimiterConfig, TokenBucketConfig};
use cube_hypervisor::vm_config::{
    DiskConfig, FsConfig, MacAddr, NetConfig, PmemConfig, VsockConfig,
};
use nix::sys::socket::{sendmsg, ControlMessage, MsgFlags};
use oci_spec::runtime::{LinuxResources, Process, Spec};
use serde::Deserialize;
use serde_json;
use tokio::{
    fs::OpenOptions,
    io::{AsyncReadExt, AsyncWriteExt},
    net::{UnixDatagram, UnixStream},
};
use ttrpc::r#async::Client;

use super::{CResult, PRODUCT_CUBEBOX};
use crate::common::{GUEST_VIRTIOFS_MNT_PATH, PAUSE_VM_SNAPSHOT_BASE};
use crate::sandbox::config::{Fs, VirtioFs};
use crate::sandbox::config::{VIRTIO_FS_ID, VIRTIO_FS_TAG};
use crate::sandbox::disk::Disk;
use crate::sandbox::net::Interface;
use crate::sandbox::pmem::Pmem;

pub const SHIM_PID_FILE: &str = "shim.pid";
pub const VMM_PID_FILE: &str = "vmm.pid";
pub const ADDRESS_FILE: &str = "address";
pub const VM_PATH: &str = "/run/vc/vm/";

pub const IMAGE_VERSION: &str = "/usr/local/services/cubetoolbox/cube-image/version";

const SNAPSHOT_BASE_DIR: &str = "/usr/local/services/cubetoolbox/cube-snapshot";

const DEV_URANDOM: &str = "/dev/urandom";

const PASSFD_CONNECT_TIMEOUT: Duration = Duration::from_secs(10);
const PASSFD_ACK_MAX_LINE_LEN: usize = 64;

fn validate_passfd_send(request_len: usize, sent: usize) -> CResult<()> {
    if sent != request_len {
        return Err(format!(
            "passfd send phase incomplete: sent {} of {} command bytes",
            sent, request_len
        ));
    }
    Ok(())
}

fn parse_passfd_responses(
    response: &[u8],
    expected_labels: &[&str],
) -> CResult<Option<HashMap<String, u32>>> {
    let expected = expected_labels.iter().copied().collect::<HashSet<_>>();
    if expected.len() != expected_labels.len() {
        return Err("passfd request contains duplicate labels".to_string());
    }
    if response.len() > expected_labels.len() * PASSFD_ACK_MAX_LINE_LEN {
        return Err("passfd ACK exceeds maximum response size".to_string());
    }

    let line_count = response.iter().filter(|byte| **byte == b'\n').count();
    if line_count < expected_labels.len() {
        return Ok(None);
    }
    if line_count > expected_labels.len() {
        return Err("passfd ACK contains extra response lines".to_string());
    }
    if !response.ends_with(b"\n") {
        return Err("passfd ACK contains trailing data".to_string());
    }

    let response = std::str::from_utf8(response)
        .map_err(|e| format!("passfd ACK contains invalid UTF-8: {}", e))?;
    let mut ports = HashMap::new();
    for line in response.lines() {
        let fields = line.split_whitespace().collect::<Vec<_>>();
        if fields.len() != 3 || fields[0] != "OK" {
            return Err(format!("invalid passfd ACK line: {:?}", line));
        }

        let label = fields[1];
        if !expected.contains(label) {
            return Err(format!("unknown passfd ACK label: {}", label));
        }
        let port = fields[2]
            .parse::<u32>()
            .map_err(|e| format!("invalid passfd ACK port for {}: {}", label, e))?;
        if port == 0 {
            return Err(format!("passfd ACK port for {} must be nonzero", label));
        }
        if ports.insert(label.to_string(), port).is_some() {
            return Err(format!("duplicate passfd ACK label: {}", label));
        }
    }

    for label in expected_labels {
        if !ports.contains_key(*label) {
            return Err(format!("missing passfd ACK for {}", label));
        }
    }
    Ok(Some(ports))
}

async fn with_passfd_connect_timeout<F, T>(future: F, timeout: Duration) -> CResult<T>
where
    F: Future<Output = CResult<T>>,
{
    tokio::time::timeout(timeout, future).await.map_err(|_| {
        format!(
            "passfd connection timed out after {} seconds",
            timeout.as_secs_f64()
        )
    })?
}

pub const NET_DEVICE_ID_PRE: &str = "tap";
pub const DISK_DEVICE_ID_PRE: &str = "disk";

// ivshmem shared memory configuration
const IVSHMEM_SHM_DIR: &str = "/dev/shm";
const IVSHMEM_PREFIX: &str = "ivshmem-";

pub struct Utils {}
pub struct AsyncUtils {}
impl Utils {
    /// Validate sandbox_id before using it in filesystem paths.
    fn validate_sandbox_id(id: &str) -> CResult<()> {
        if id.is_empty() || id.len() > 255 {
            return Err("invalid sandbox_id length".into());
        }
        if id.contains("..") || id.contains('/') || id.contains('\\') {
            return Err("sandbox_id contains invalid path characters".into());
        }
        Ok(())
    }

    /// Return `/dev/shm/ivshmem-{sandbox_id}` for the sandbox.
    pub fn ivshmem_path(sandbox_id: &str) -> CResult<PathBuf> {
        Self::validate_sandbox_id(sandbox_id)?;
        Ok(PathBuf::from(format!(
            "{}/{}{}",
            IVSHMEM_SHM_DIR, IVSHMEM_PREFIX, sandbox_id
        )))
    }

    /// Create or truncate an ivshmem backend file with 0o600 permissions.
    pub fn create_ivshmem_file(path: &Path, size: usize) -> CResult<()> {
        let f = File::options()
            .create(true)
            .write(true)
            .truncate(true)
            .mode(0o600)
            .open(path)
            .map_err(|e| format!("failed to create ivshmem file {}: {}", path.display(), e))?;

        f.set_len(size as u64)
            .map_err(|e| format!("failed to set ivshmem file size: {}", e))?;

        Ok(())
    }

    pub fn load_spec(bundle: &str) -> CResult<Spec> {
        let mut conf_path = PathBuf::from(bundle);
        conf_path.push("config.json");
        let spec = Spec::load(conf_path)
            .map_err(|e| format!("load config failed:{} bundle:{}", e, bundle))?;
        Ok(spec)
    }

    pub fn record_pid() -> CResult<()> {
        let pid = process::id();
        let mut file =
            File::create(SHIM_PID_FILE).map_err(|e| format!("Create pid file failed:{}", e))?;
        file.write_all(pid.to_string().as_bytes())
            .map_err(|e| format!("Write pid file failed:{}", e))?;

        let mut file =
            File::create(VMM_PID_FILE).map_err(|e| format!("Create pid file failed:{}", e))?;
        file.write_all(pid.to_string().as_bytes())
            .map_err(|e| format!("Write pid file failed:{}", e))?;
        Ok(())
    }

    pub fn get_oci_proc(data: &[u8]) -> CResult<Process> {
        let p = serde_json::from_slice::<oci_spec::runtime::Process>(data)
            .map_err(|e| format!("deserialize process failed:{}", e))?;
        Ok(p)
    }

    pub fn get_oci_res(data: &[u8]) -> CResult<LinuxResources> {
        let r = serde_json::from_slice::<oci_spec::runtime::LinuxResources>(data)
            .map_err(|e| format!("deserialize resource failed:{}", e))?;
        Ok(r)
    }

    pub fn get_kernel_version(kernel_path: &str) -> CResult<String> {
        let kernel = PathBuf::from(kernel_path);
        let mut ker_version = kernel.parent().unwrap().to_path_buf();
        ker_version.push("version");

        let mut file = File::open(ker_version.clone())
            .map_err(|e| format!("open file {:?} failed:{}", ker_version.clone(), e))?;

        let mut version = String::new();

        file.read_to_string(&mut version)
            .map_err(|e| format!("read file {:?} failed:{}", ker_version, e))?;
        Ok(version.trim().to_string())
    }

    pub fn get_image_version() -> CResult<String> {
        let mut file = File::open(IMAGE_VERSION)
            .map_err(|e| format!("open file {} failed:{}", IMAGE_VERSION, e))?;

        let mut version = String::new();
        file.read_to_string(&mut version)
            .map_err(|e| format!("read file {} failed:{}", IMAGE_VERSION, e))?;
        Ok(version.trim().to_string())
    }

    /// Read version file next to the OS image plane file
    /// (`…/cube-image/cube-guest-image-cpu.img` → `…/cube-image/version`).
    pub fn get_image_version_for_path(image_path: &str) -> CResult<String> {
        let image = PathBuf::from(image_path);
        let img_version = image
            .parent()
            .map(|p| p.join("version"))
            .ok_or_else(|| format!("invalid os image path (no parent): {}", image_path))?;

        let mut file = File::open(img_version.clone())
            .map_err(|e| format!("open file {:?} failed:{}", img_version.clone(), e))?;

        let mut version = String::new();
        file.read_to_string(&mut version)
            .map_err(|e| format!("read file {:?} failed:{}", img_version, e))?;
        Ok(version.trim().to_string())
    }

    /// Read version file next to cube-agent.ext4
    /// (`…/cube-agent/cube-agent.ext4` → `…/cube-agent/version`).
    pub fn get_agent_version(agent_path: &str) -> CResult<String> {
        let agent = PathBuf::from(agent_path);
        let ver_path = agent
            .parent()
            .map(|p| p.join("version"))
            .ok_or_else(|| format!("invalid agent path (no parent): {}", agent_path))?;

        let mut file = File::open(ver_path.clone())
            .map_err(|e| format!("open file {:?} failed:{}", ver_path.clone(), e))?;

        let mut version = String::new();
        file.read_to_string(&mut version)
            .map_err(|e| format!("read file {:?} failed:{}", ver_path, e))?;
        Ok(version.trim().to_string())
    }

    pub fn get_snapshot_base_dir(base: Option<&str>, _product: &str) -> String {
        if let Some(base_dir) = base {
            return base_dir.to_string();
        }
        let mut pb = PathBuf::from(SNAPSHOT_BASE_DIR);
        pb.push(PRODUCT_CUBEBOX);
        pb.to_str().unwrap().to_string()
    }

    pub fn get_snapshot_metadata_file(base: &str, cpu: u32, memory: u64) -> String {
        let mut pb = PathBuf::from(base);
        pb.push(Self::get_snapshot_res_dir(cpu, memory));
        pb.push("metadata.json");

        pb.to_str().unwrap().to_string()
    }

    pub fn get_snapshot_dir(base: &str, cpu: u32, memory: u64) -> String {
        let mut pb = PathBuf::from(base);
        pb.push(Self::get_snapshot_res_dir(cpu, memory));
        pb.push("snapshot");
        format!("file://{}", pb.to_str().unwrap())
    }

    fn get_snapshot_res_dir(cpu: u32, memory: u64) -> String {
        format!("{}C{}M", cpu, memory)
    }

    pub fn get_rng() -> CResult<Vec<u8>> {
        let mut file =
            File::open(DEV_URANDOM).map_err(|e| format!("open {} failed:{}", DEV_URANDOM, e))?;
        let mut buf = [0u8, 0];
        file.read_exact(&mut buf)
            .map_err(|e| format!("read {} failed:{}", DEV_URANDOM, e))?;

        Ok(buf.to_vec())
    }

    pub fn clean_sandbox_resource(sandbox_id: &String) -> CResult<()> {
        //delete vmm workdir
        let vm_dir = PathBuf::from(VM_PATH).join(sandbox_id);
        let ret_vmdir: Result<(), String> = match fs::remove_dir_all(&vm_dir) {
            Ok(_) => Ok(()),
            Err(e) if e.kind() == ErrorKind::NotFound => Ok(()),
            Err(e) => Err(format!("del {} failed:{}", vm_dir.display(), e.to_string())),
        };

        //delete pause vm snapshot dir
        let pause_dir = PathBuf::from(PAUSE_VM_SNAPSHOT_BASE).join(sandbox_id);
        let ret_pausedir: Result<(), String> = match fs::remove_dir_all(&pause_dir) {
            Ok(_) => Ok(()),
            Err(e) if e.kind() == ErrorKind::NotFound => Ok(()),
            Err(e) => Err(format!(
                "del {} failed:{}",
                pause_dir.display(),
                e.to_string()
            )),
        };

        //delete ivshmem shared memory file
        let ret_ivshmem: Result<(), String> = match Self::ivshmem_path(sandbox_id) {
            Ok(ivshmem_path) => match fs::remove_file(&ivshmem_path) {
                Ok(_) => Ok(()),
                Err(e) if e.kind() == ErrorKind::NotFound => Ok(()),
                Err(e) => Err(format!(
                    "failed to delete {}: {}",
                    ivshmem_path.display(),
                    e
                )),
            },
            Err(e) => Err(format!("resolve ivshmem path failed:{}", e)),
        };

        // Regular-sandbox host logs. Template builds write under
        // /data/log/template and never create this dir, so this is a no-op
        // for app_snapshot_create. Task.Delete on a live shim is not enough:
        // after a crash containerd reaps via the delete subcommand.
        let ret_logdir = crate::container::remove_sandbox_log_dir_sync(sandbox_id);

        let mut err_msg = String::new();
        if let Err(e) = ret_vmdir {
            err_msg = err_msg + e.as_str();
        }

        if let Err(e) = ret_pausedir {
            err_msg = err_msg + e.as_str();
        }

        if let Err(e) = ret_ivshmem {
            err_msg = err_msg + e.as_str();
        }

        if let Err(e) = ret_logdir {
            err_msg = err_msg + e.as_str();
        }

        if err_msg.is_empty() {
            return Ok(());
        }

        Err(err_msg)
    }

    pub fn restore_fs_configs(fs: &Fs) -> FsConfig {
        //these value copied from the shim implementation in Go

        FsConfig {
            id: Some(VIRTIO_FS_ID.to_string()),
            tag: VIRTIO_FS_TAG.to_string(),
            num_queues: 1, //
            queue_size: 1024,
            backendfs_config: fs.backendfs_config.clone(),
            rate_limiter_config: fs.rate_limiter_config,
            ..Default::default()
        }
    }

    pub fn restore_virtiofs_configs(fs: &Vec<VirtioFs>) -> Vec<FsConfig> {
        //these value copied from the shim implementation in Go
        let mut fs_configs = Vec::<FsConfig>::new();
        for fs in fs.iter() {
            let fs_config = FsConfig {
                id: Some(fs.id.clone()),
                tag: fs.id.clone(),
                num_queues: 1, //
                queue_size: 1024,
                backendfs_config: fs.backendfs_config.clone(),
                rate_limiter_config: fs.rate_limiter_config,
                ..Default::default()
            };
            fs_configs.push(fs_config);
        }
        fs_configs
    }

    pub fn restore_nets_config(nets: &[Interface]) -> CResult<Vec<NetConfig>> {
        let mut net_configs = Vec::<NetConfig>::new();
        for (i, n) in nets.iter().enumerate() {
            let mac =
                MacAddr::from_str(&n.mac).map_err(|_| format!("New mac addr failed:{}", &n.mac))?;
            let mut net_config = NetConfig {
                tap: n.name.clone(),
                mac,
                id: Some(format!("{}-{}", NET_DEVICE_ID_PRE, i)),
                ..Default::default()
            };

            if let Some(qos) = &n.qos {
                net_config.rate_limiter_config = Some(RateLimiterConfig {
                    bandwidth: Some(TokenBucketConfig {
                        size: qos.bw_size,
                        one_time_burst: Some(qos.bw_one_time_burst),
                        refill_time: qos.bw_refill_time,
                    }),
                    ops: Some(TokenBucketConfig {
                        size: qos.ops_size,
                        one_time_burst: Some(qos.ops_one_time_burst),
                        refill_time: qos.ops_refill_time,
                    }),
                })
            }
            net_configs.push(net_config);
        }
        Ok(net_configs)
    }

    pub fn restore_disks_config(disks: &Vec<Disk>) -> Vec<DiskConfig> {
        let mut disk_configs = Vec::<DiskConfig>::new();
        for d in disks {
            let disk_config = DiskConfig {
                id: Some(format!("{}-{}", DISK_DEVICE_ID_PRE, disk_configs.len())),
                path: Some(PathBuf::from(d.path.clone())),
                rate_limiter_config: d.rate_limiter_config,
                ..Default::default()
            };
            disk_configs.push(disk_config);
        }
        disk_configs
    }

    pub fn restore_pmems_config(pmems: &Vec<Pmem>) -> Vec<PmemConfig> {
        let mut pmem_configs = Vec::<PmemConfig>::new();

        for p in pmems {
            let pmem_config = PmemConfig {
                file: PathBuf::from(p.file.clone()),
                discard_writes: p.discard_writes,
                size: p.size,
                id: Some(p.id.clone()),
                ..Default::default()
            };
            pmem_configs.push(pmem_config);
        }

        pmem_configs
    }

    pub fn vsock_path(sandbox_id: &str) -> PathBuf {
        PathBuf::from(format!("{}/{}/cube.sock", VM_PATH, sandbox_id))
    }

    pub fn chapi_path(sandbox_id: &str) -> String {
        format!("{}/{}/chapi", VM_PATH, sandbox_id)
    }

    pub fn gen_vsock_config(sandbox_id: &str) -> VsockConfig {
        let sock_file = Self::vsock_path(sandbox_id);

        VsockConfig {
            cid: 3,
            socket: sock_file,
            id: Some("vsock".to_string()),
            ..Default::default()
        }
    }

    pub fn anno_to_obj<'a, T: Deserialize<'a>>(anno: &'a String) -> CResult<T> {
        let tobj = serde_json::from_str(anno)
            .map_err(|e| format!("Deserializa to Obj failed:{} anno:{}", e, anno))?;
        Ok(tobj)
    }

    /// 将PCI BDF地址转换为slot.function格式
    /// 输入示例: "0000:00:07.0"
    /// 输出示例: "07.0"
    pub fn bdf_to_slotfn(bdf: &str) -> Option<String> {
        let parts: Vec<&str> = bdf.split(':').collect();
        if parts.len() != 3 {
            return None;
        }

        // 提取最后一个部分，即slot.function
        let slot_fn = parts[2];

        // 验证格式
        let slot_fn_parts: Vec<&str> = slot_fn.split('.').collect();
        if slot_fn_parts.len() != 2 {
            return None;
        }

        // 确保slot和function都是有效的十六进制数
        if u8::from_str_radix(slot_fn_parts[0], 16).is_err()
            || u8::from_str_radix(slot_fn_parts[1], 16).is_err()
        {
            return None;
        }

        Some(slot_fn.to_string())
    }

    pub fn virtiofs_guest_base(virtiofs_id: String) -> String {
        format!("{}/{}", GUEST_VIRTIOFS_MNT_PATH, virtiofs_id)
    }
}

impl AsyncUtils {
    pub const PASSFD_LISTENER_PORT: u32 = 1027;

    pub async fn connect_agent(sandbox_id: &String) -> CResult<Client> {
        let addr = Utils::vsock_path(sandbox_id);

        let mut stream = UnixStream::connect(&addr)
            .await
            .map_err(|e| format!("Connect agent failed at {:?}: {}", addr, e))?;
        let req = "CONNECT 1024\n";
        stream
            .write_all(req.as_bytes())
            .await
            .map_err(|e| format!("Send connect cmd failed:{}", e))?;

        let mut buffer = Vec::new();
        let len = stream
            .read_buf(&mut buffer)
            .await
            .map_err(|e| format!("Recv connect rsp failed:{}", e))?;
        if len < 2 {
            return Err(format!("Recv len invalid:{}", len));
        }
        let rsp = String::from_utf8(buffer[..len].to_vec()).unwrap();

        if !rsp.contains("OK") {
            return Err(format!("Connect failed rsp:{}", rsp));
        }

        let nfd = nix::unistd::dup(stream.as_raw_fd()).unwrap();
        std::mem::drop(stream);

        let conn = Client::new(nfd);
        Ok(conn)
    }

    pub async fn passfd_connect_many(
        sandbox_id: &String,
        peer_port: u32,
        streams: &[(&str, std::os::unix::io::RawFd)],
    ) -> CResult<(u32, u32, u32)> {
        if streams.is_empty() {
            return Err("passfd stream list is empty".to_string());
        }

        let addr = Utils::vsock_path(sandbox_id);

        let mut stream = tokio::net::UnixStream::connect(&addr)
            .await
            .map_err(|e| format!("passfd connect phase failed at {:?}: {}", addr, e))?;

        let labels = streams
            .iter()
            .map(|(label, _)| *label)
            .collect::<Vec<_>>()
            .join(" ");
        let req = format!("passfd {} {}\n", peer_port, labels);
        let fds = streams.iter().map(|(_, fd)| *fd).collect::<Vec<_>>();
        let cmsg = ControlMessage::ScmRights(&fds);
        let iov = [IoSlice::new(req.as_bytes())];

        let raw_stream_fd = stream.as_raw_fd();
        let sent = stream
            .async_io(tokio::io::Interest::WRITABLE, || {
                sendmsg::<()>(raw_stream_fd, &iov, &[cmsg], MsgFlags::empty(), None)
                    .map_err(|e| std::io::Error::from_raw_os_error(e as i32))
            })
            .await
            .map_err(|e| format!("passfd send phase failed: {}", e))?;
        validate_passfd_send(req.len(), sent)?;

        let expected_labels = streams.iter().map(|(label, _)| *label).collect::<Vec<_>>();
        let mut rsp = Vec::new();
        let mut buf_slice = [0u8; 64];
        let ports = loop {
            if let Some(ports) = parse_passfd_responses(&rsp, &expected_labels)? {
                break ports;
            }
            let len = tokio::io::AsyncReadExt::read(&mut stream, &mut buf_slice)
                .await
                .map_err(|e| format!("passfd ACK wait phase failed: {}", e))?;
            if len == 0 {
                return Err("passfd ACK wait phase: control socket closed early".to_string());
            }
            rsp.extend_from_slice(&buf_slice[..len]);
        };

        let stdin_port = ports.get("stdin").copied().unwrap_or(0);
        let stdout_port = ports.get("stdout").copied().unwrap_or(0);
        let stderr_port = ports.get("stderr").copied().unwrap_or(0);

        Ok((stdin_port, stdout_port, stderr_port))
    }

    pub async fn setup_passfd_streams(
        sandbox_id: &String,
        stdin_path: &str,
        stdout_path: &str,
        stderr_path: &str,
    ) -> CResult<(u32, u32, u32)> {
        let mut files = Vec::new();
        let mut stream_fds = Vec::new();
        let mut dummy_writer = None;

        if !stdin_path.is_empty() {
            let file = OpenOptions::new()
                .read(true)
                .custom_flags(libc::O_NONBLOCK)
                .open(stdin_path)
                .await
                .map_err(|e| format!("open stdin error: {}", e))?;

            dummy_writer = OpenOptions::new()
                .write(true)
                .custom_flags(libc::O_NONBLOCK)
                .open(stdin_path)
                .await
                .ok();

            stream_fds.push(("stdin", file.as_raw_fd()));
            files.push(file);
        }
        if !stdout_path.is_empty() {
            let file = OpenOptions::new()
                .write(true)
                .custom_flags(libc::O_NONBLOCK)
                .open(stdout_path)
                .await
                .map_err(|e| format!("open stdout error: {}", e))?;
            stream_fds.push(("stdout", file.as_raw_fd()));
            files.push(file);
        }
        if !stderr_path.is_empty() {
            let file = OpenOptions::new()
                .write(true)
                .custom_flags(libc::O_NONBLOCK)
                .open(stderr_path)
                .await
                .map_err(|e| format!("open stderr error: {}", e))?;
            stream_fds.push(("stderr", file.as_raw_fd()));
            files.push(file);
        }

        if stream_fds.is_empty() {
            return Ok((0, 0, 0));
        }

        let (stdin_port, stdout_port, stderr_port) = with_passfd_connect_timeout(
            Self::passfd_connect_many(sandbox_id, Self::PASSFD_LISTENER_PORT, &stream_fds),
            PASSFD_CONNECT_TIMEOUT,
        )
        .await?;
        drop(dummy_writer);
        drop(files);

        Ok((stdin_port, stdout_port, stderr_port))
    }

    pub async fn notify_snapshot_ret(sandbox_id: &String, snapshot: bool) -> CResult<()> {
        let addr = PathBuf::from("/tmp/health-check.sock");
        let msg = format!(
            r#"{{"sandboxID":"{}","restoreVm":{}}}"#,
            sandbox_id, snapshot
        );
        let socket =
            UnixDatagram::unbound().map_err(|e| format!("create udp socket failed:{}", e))?;
        socket
            .send_to(msg.as_bytes(), &addr)
            .await
            .map_err(|e| format!("send data failed:{}", e))?;
        Ok(())
    }
}

#[derive(Debug)]
pub struct CPath {
    pub path: PathBuf,
}

impl CPath {
    pub fn new(p: &str) -> Self {
        CPath {
            path: PathBuf::from(p),
        }
    }

    pub fn join(&mut self, p: &str) -> &mut Self {
        if let Some(stripped) = p.strip_prefix('/') {
            self.path.push(stripped);
        } else {
            self.path.push(p);
        }
        self
    }

    pub fn to_str(&self) -> Option<&str> {
        self.path.to_str()
    }

    pub fn to_path_buf(&self) -> PathBuf {
        self.path.clone()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn passfd_send_requires_one_complete_sendmsg() {
        assert!(validate_passfd_send(27, 27).is_ok());
        assert!(validate_passfd_send(27, 0).is_err());
        assert!(validate_passfd_send(27, 7).is_err());
        assert!(validate_passfd_send(27, 28).is_err());
    }

    #[test]
    fn passfd_ack_parser_handles_fragmented_complete_response() {
        let labels = ["stdin", "stdout"];
        assert!(parse_passfd_responses(b"OK stdin 1001\nOK std", &labels)
            .unwrap()
            .is_none());

        let ports = parse_passfd_responses(b"OK stdin 1001\nOK stdout 1002\n", &labels)
            .unwrap()
            .unwrap();
        assert_eq!(ports.get("stdin"), Some(&1001));
        assert_eq!(ports.get("stdout"), Some(&1002));
    }

    #[test]
    fn passfd_ack_parser_rejects_invalid_responses() {
        let cases: &[(&[u8], &[&str])] = &[
            (b"OK stdin 1001\nOK stdin 1002\n", &["stdin", "stdout"]),
            (b"OK unknown 1001\n", &["stdin"]),
            (b"OK stdin 0\n", &["stdin"]),
            (b"OK stdin invalid\n", &["stdin"]),
            (b"OK stdin 1001 extra\n", &["stdin"]),
            (b"OK stdin 1001\nOK stdout 1002\n", &["stdin"]),
            (b"OK stdin 1001\ntrailing", &["stdin"]),
            (b"ERR stdin 1001\n", &["stdin"]),
            (b"OK \xff 1001\n", &["stdin"]),
        ];
        for (response, labels) in cases {
            assert!(
                parse_passfd_responses(response, labels).is_err(),
                "unexpectedly accepted {:?}",
                response
            );
        }

        assert!(parse_passfd_responses(b"OK stdin 1001\n", &["stdin", "stdin"]).is_err());
        assert!(parse_passfd_responses(&vec![b'x'; 65], &["stdin"]).is_err());
    }

    #[tokio::test]
    async fn passfd_connect_timeout_is_bounded() {
        let result: CResult<()> =
            with_passfd_connect_timeout(std::future::pending(), Duration::from_millis(10)).await;

        assert_eq!(
            result.unwrap_err(),
            "passfd connection timed out after 0.01 seconds"
        );
    }

    #[test]
    fn cpath() {
        let mut cp = CPath::new("/a/b/c");
        cp.join("/d/e");

        //to_str
        let strp = cp.to_str();
        assert!(strp.is_some());
        let p = strp.unwrap();
        assert_eq!(p, "/a/b/c/d/e");

        //to_path_buf
        let p = cp.to_path_buf();
        let strp = p.to_str();
        assert!(strp.is_some());
        let p = strp.unwrap().to_string();
        assert_eq!(p, "/a/b/c/d/e");
    }

    #[test]
    fn utils_get_snapshot_base_dir() {
        let cubebox = format!("{}/{}", SNAPSHOT_BASE_DIR, PRODUCT_CUBEBOX);
        assert_eq!(
            Utils::get_snapshot_base_dir(Some("/123"), "test"),
            "/123".to_string()
        );
        assert_eq!(Utils::get_snapshot_base_dir(None, PRODUCT_CUBEBOX), cubebox);
    }

    #[test]
    fn utils_get_snapshot_metadata_file() {
        let ret = format!("/123/{}C{}M/metadata.json", 1, 1);

        assert_eq!(Utils::get_snapshot_metadata_file("/123", 1, 1), ret);
    }

    #[test]
    fn utils_get_snapshot_dir() {
        let ret = format!("file:///123/{}C{}M/snapshot", 1, 1);

        assert_eq!(Utils::get_snapshot_dir("/123", 1, 1), ret);
    }

    #[test]
    fn utils_get_snapshot_res_dir() {
        assert_eq!(Utils::get_snapshot_res_dir(1, 1), "1C1M");
    }

    #[test]
    fn utils_restore_fs_configs() {
        let fs = Fs {
            backendfs_config: None,
            rate_limiter_config: None,
        };

        let config1 = FsConfig {
            id: Some(VIRTIO_FS_ID.to_string()),
            tag: VIRTIO_FS_TAG.to_string(),
            num_queues: 1, //
            queue_size: 1024,
            backendfs_config: fs.backendfs_config.clone(),
            rate_limiter_config: fs.rate_limiter_config,
            ..Default::default()
        };
        let config2 = Utils::restore_fs_configs(&fs);
        assert_eq!(config1, config2);
    }

    #[test]
    fn utils_restore_nets_config() {
        let interfaces = vec![Interface {
            name: Some("ut1".to_string()),
            mac: "xxxx".to_string(),
            ..Default::default()
        }];
        let nets = Utils::restore_nets_config(&interfaces);
        assert!(nets.is_err());
    }

    #[test]
    fn utils_restore_disks_config() {
        let disks = vec![Disk {
            path: "/dev/test".to_string(),
            source_dir: "/".to_string(),
            fs_type: "ext4".to_string(),
            size: 1024,
            fs_quota: 1024,
            rate_limiter_config: None,
        }];
        let disk_configs = Utils::restore_disks_config(&disks);
        assert_eq!(disk_configs.len(), disks.len());

        let exp_path = Some(PathBuf::from(disks[0].path.clone()));
        assert_eq!(disk_configs[0].path, exp_path);
        assert_eq!(
            disk_configs[0].id,
            Some(format!("{}-{}", DISK_DEVICE_ID_PRE, disk_configs.len() - 1))
        )
    }

    #[test]
    fn utils_restore_pmems_config() {
        let pmems = vec![Pmem {
            file: "/dev/pmem".to_string(),
            source_dir: "/".to_string(),
            fs_type: "ext4".to_string(),
            size: Some(1024),
            id: "1".to_string(),
            ..Default::default()
        }];

        let pmem_config: Vec<PmemConfig> = Utils::restore_pmems_config(&pmems);
        assert_eq!(pmem_config.len(), pmems.len());
        assert_eq!(pmem_config[0].file, PathBuf::from("/dev/pmem"));
        assert_eq!(pmem_config[0].size, Some(1024));
        assert_eq!(pmem_config[0].id, Some("1".to_string()));
    }

    #[test]
    fn utils_vsock_path() {
        let p = Utils::vsock_path("123");
        assert_eq!(p, PathBuf::from(format!("{}/{}/cube.sock", VM_PATH, "123")));
    }

    #[test]
    fn utils_gen_vsock_config() {
        let config = Utils::gen_vsock_config("123");
        assert_eq!(config.cid, 3);
        assert_eq!(config.id.unwrap(), "vsock".to_string());
        assert_eq!(config.socket, Utils::vsock_path("123"))
    }

    #[test]
    fn test_bdf_to_slotfn() {
        assert_eq!(
            Utils::bdf_to_slotfn("0000:00:07.0"),
            Some("07.0".to_string())
        );
        assert_eq!(
            Utils::bdf_to_slotfn("ffff:ff:1f.7"),
            Some("1f.7".to_string())
        );
        assert_eq!(
            Utils::bdf_to_slotfn("0000:00:00.0"),
            Some("00.0".to_string())
        );
        assert_eq!(
            Utils::bdf_to_slotfn("0000:00:1a.1"),
            Some("1a.1".to_string())
        );

        // 无效格式
        assert_eq!(Utils::bdf_to_slotfn("0000:00:07"), None); // 缺少function
        assert_eq!(Utils::bdf_to_slotfn("0000:00:07.0.0"), None); // 格式错误
        assert_eq!(Utils::bdf_to_slotfn("0000:00:xy.0"), None); // 无效十六进制
    }

    #[test]
    fn utils_virtiofs_guest_base() {
        let base = Utils::virtiofs_guest_base("123".to_string());
        assert_eq!(base, format!("{}/{}", GUEST_VIRTIOFS_MNT_PATH, "123"));
    }

    // ivshmem path validation tests
    #[test]
    fn test_ivshmem_path_valid() {
        let path = Utils::ivshmem_path("sb-12345").unwrap();
        assert_eq!(path, PathBuf::from("/dev/shm/ivshmem-sb-12345"));
    }

    #[test]
    fn test_ivshmem_path_empty() {
        let result = Utils::ivshmem_path("");
        assert!(result.is_err());
        assert!(result.unwrap_err().contains("invalid sandbox_id length"));
    }

    #[test]
    fn test_ivshmem_path_too_long() {
        let long_id = "a".repeat(256);
        let result = Utils::ivshmem_path(&long_id);
        assert!(result.is_err());
        assert!(result.unwrap_err().contains("invalid sandbox_id length"));
    }

    #[test]
    fn test_ivshmem_path_traversal_dotdot() {
        let result = Utils::ivshmem_path("../etc/passwd");
        assert!(result.is_err());
        assert!(result.unwrap_err().contains("invalid path characters"));
    }

    #[test]
    fn test_ivshmem_path_traversal_slash() {
        let result = Utils::ivshmem_path("foo/bar");
        assert!(result.is_err());
        assert!(result.unwrap_err().contains("invalid path characters"));
    }

    #[test]
    fn test_ivshmem_path_traversal_backslash() {
        let result = Utils::ivshmem_path("foo\\bar");
        assert!(result.is_err());
        assert!(result.unwrap_err().contains("invalid path characters"));
    }

    #[test]
    fn test_ivshmem_path_embedded_dotdot() {
        let result = Utils::ivshmem_path("foo..bar");
        assert!(result.is_err());
        assert!(result.unwrap_err().contains("invalid path characters"));
    }

    #[test]
    fn test_ivshmem_path_max_length() {
        let id = "a".repeat(255);
        let result = Utils::ivshmem_path(&id);
        assert!(result.is_ok());
        assert_eq!(
            result.unwrap(),
            PathBuf::from(format!("/dev/shm/ivshmem-{}", "a".repeat(255)))
        );
    }

    #[test]
    fn test_create_ivshmem_file_success() {
        use std::os::unix::fs::PermissionsExt;

        let temp_dir = std::env::temp_dir();
        let test_file = temp_dir.join(format!("test-ivshmem-{}", std::process::id()));

        // Clean up in case of previous failed test
        let _ = fs::remove_file(&test_file);

        // Create file
        let result = Utils::create_ivshmem_file(&test_file, 1024 * 1024);
        assert!(result.is_ok(), "Failed to create file: {:?}", result.err());

        // Verify file exists
        assert!(test_file.exists(), "File was not created");

        // Verify size
        let metadata = fs::metadata(&test_file).unwrap();
        assert_eq!(metadata.len(), 1024 * 1024, "File size incorrect");

        // Verify permissions (0o600)
        let mode = metadata.permissions().mode();
        assert_eq!(
            mode & 0o777,
            0o600,
            "File permissions incorrect: {:o}",
            mode & 0o777
        );

        // Clean up
        fs::remove_file(&test_file).unwrap();
    }

    #[test]
    fn test_create_ivshmem_file_invalid_path() {
        let invalid_path = PathBuf::from("/nonexistent/impossible/directory/test-ivshmem");
        let result = Utils::create_ivshmem_file(&invalid_path, 1024);

        assert!(result.is_err(), "Should fail with invalid path");
        assert!(result
            .unwrap_err()
            .contains("failed to create ivshmem file"));
    }
}
