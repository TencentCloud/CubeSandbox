// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! `cube-runtime verify-restore`: restore a pause package into a throwaway VM
//! and prove the guest agent answers, without touching anything a production
//! sandbox uses.
//!
//! The caller (Cubelet, on a node other than the package's origin) puts the
//! process in a fresh network namespace before exec, downloads the package
//! into private files, and passes their paths here. This command refuses to
//! restore unless it really is isolated, and rewrites every host-side path
//! the snapshot recorded: writable disks to the private copies, vsock and
//! virtiofs to a private directory. Read-only images (kernel, OS image,
//! agent, container image pmem) are used in place and must match the
//! versions the snapshot was taken with, the same check a production
//! restore makes.

use std::collections::HashMap;
use std::fs;
use std::path::{Path, PathBuf};
use std::sync::mpsc::{channel, Receiver};
use std::sync::Arc;
use std::time::{Duration, Instant};

use chrono::Utc;
use clap::Args;
use cube_hypervisor::config::RestoreConfig;
use cube_hypervisor::vm_config::VmConfig as ChVmConfig;
use cube_hypervisor::{vmm_config, ApiRequest, NotifyEvent, VmmInstance};
use protoc::{agent, agent_ttrpc};
use serde::Serialize;
use ttrpc::context;

use crate::common::utils::{AsyncUtils, Utils, VM_PATH};
use crate::common::CResult;
use crate::hypervisor::snapshot::SnapshotInfo;
use crate::sandbox::pmem::{HYP_AGENT_ID, HYP_OS_IMAGE_ID};

use super::FilePtr;

/// Overall budget for the agent handshake and reset RPCs, as on resume.
const AGENT_BUDGET: Duration = Duration::from_secs(15);
const AGENT_ATTEMPT_TIMEOUT: Duration = Duration::from_secs(3);
const AGENT_RPC_TIMEOUT_NS: i64 = 3_000_000_000;

#[derive(Args, Debug)]
pub struct VerifyRestoreArgs {
    /// Package home: holds <cpu>C<mem>M/{metadata.json,snapshot/}.
    #[arg(long = "base", required = true)]
    pub base: String,
    /// Identity of the throwaway VM; also names its private run directory.
    #[arg(long = "vm-id", required = true)]
    pub vm_id: String,
    #[arg(long = "cpu", required = true)]
    pub cpu: u32,
    /// Memory in MiB.
    #[arg(long = "memory", required = true)]
    pub memory: u64,
    /// Private copy of the guest memory image.
    #[arg(long = "memory-vol", required = true)]
    pub memory_vol: String,
    /// `<recorded disk path>=<private copy>`; every writable disk the
    /// snapshot recorded must be mapped.
    #[arg(long = "disk", required = true)]
    pub disks: Vec<String>,
    /// Empty directory served to every virtiofs share instead of host data.
    #[arg(long = "share-dir", required = true)]
    pub share_dir: String,
}

#[derive(Serialize, Default)]
pub struct VerifyResult {
    pub ok: bool,
    /// Stage that failed: isolation, compat, deps, config, infra, restore,
    /// agent. Only config and restore describe the package; the others say
    /// this node or this moment could not run the trial.
    pub stage: String,
    pub error: String,
    pub restore_ms: u128,
    pub vsock_ready_ms: u128,
    pub agent_ms: u128,
    pub agent_attempts: u32,
    pub netns_interfaces: Vec<String>,
}

pub const RESULT_PREFIX: &str = "VERIFY_RESULT ";

fn fail(res: &mut VerifyResult, stage: &str, err: String) {
    res.ok = false;
    res.stage = stage.to_string();
    res.error = err;
}

pub async fn execute(args: VerifyRestoreArgs) -> anyhow::Result<()> {
    let mut res = VerifyResult::default();
    let outcome = run(&args, &mut res).await;
    if let Err((stage, err)) = outcome {
        fail(&mut res, &stage, err);
    } else {
        res.ok = true;
    }
    println!(
        "{}{}",
        RESULT_PREFIX,
        serde_json::to_string(&res).unwrap_or_default()
    );
    if !res.ok {
        std::process::exit(2);
    }
    Ok(())
}

type StageResult<T> = Result<T, (String, String)>;

fn stage<T>(name: &str, r: CResult<T>) -> StageResult<T> {
    r.map_err(|e| (name.to_string(), e))
}

/// The netns check reads /proc/self, which always reflects the caller's own
/// namespace (/sys/class/net would show the namespace sysfs was mounted in).
fn check_isolation(res: &mut VerifyResult) -> CResult<()> {
    let own = fs::read_link("/proc/self/ns/net").map_err(|e| format!("read own netns: {}", e))?;
    let init = fs::read_link("/proc/1/ns/net").map_err(|e| format!("read init netns: {}", e))?;
    if own == init {
        return Err("refusing to restore in the host network namespace".to_string());
    }
    let dev =
        fs::read_to_string("/proc/self/net/dev").map_err(|e| format!("read net/dev: {}", e))?;
    let ifaces: Vec<String> = dev
        .lines()
        .skip(2)
        .filter_map(|l| l.split(':').next().map(|s| s.trim().to_string()))
        .filter(|s| !s.is_empty())
        .collect();
    res.netns_interfaces = ifaces.clone();
    if ifaces.iter().any(|i| i != "lo") {
        return Err(format!(
            "network namespace is not empty before restore: {:?}",
            ifaces
        ));
    }
    Ok(())
}

fn parse_disk_map(args: &[String]) -> CResult<HashMap<PathBuf, PathBuf>> {
    let mut out = HashMap::new();
    for a in args {
        let (from, to) = a
            .split_once('=')
            .ok_or_else(|| format!("disk mapping {:?} is not <from>=<to>", a))?;
        let to = PathBuf::from(to);
        if !to.is_file() {
            return Err(format!("private disk copy {} is missing", to.display()));
        }
        out.insert(PathBuf::from(from), to);
    }
    Ok(out)
}

/// Checks the trial's VM id. Cubelet generates it on this node, so a bad id
/// says nothing about the package: infra, which is retried elsewhere.
fn check_vm_id(vm_id: &str) -> StageResult<()> {
    if vm_id.is_empty()
        || !vm_id
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || c == '-' || c == '_')
    {
        return Err(("infra".into(), format!("invalid vm id {:?}", vm_id)));
    }
    Ok(())
}

/// Creates the trial's run directory under root; it is removed when the
/// guard drops. Failing to create it (a full or read-only /run) is a problem
/// of this node, not of the package: infra.
fn trial_run_dir(root: &Path, vm_id: &str) -> StageResult<FilePtr> {
    let dir = root.join(vm_id);
    stage("infra", FilePtr::new(dir.to_str().unwrap_or_default()))
}

fn file_size(p: &Path) -> CResult<u64> {
    fs::metadata(p)
        .map(|m| m.len())
        .map_err(|e| format!("{}: {}", p.display(), e))
}

/// Builds the restore overrides from the VM config the snapshot recorded.
fn restore_config(args: &VerifyRestoreArgs, rec: &ChVmConfig) -> StageResult<RestoreConfig> {
    if rec.devices.as_ref().map_or(false, |d| !d.is_empty())
        || rec.user_devices.as_ref().map_or(false, |d| !d.is_empty())
        || rec.vdpa.as_ref().map_or(false, |d| !d.is_empty())
    {
        return Err((
            "config".into(),
            "snapshot uses passthrough/vdpa/user devices, which cannot be restored elsewhere"
                .into(),
        ));
    }
    if rec.ivshmem.is_some() {
        return Err((
            "config".into(),
            "snapshot uses ivshmem shared memory, which cannot be restored elsewhere".into(),
        ));
    }
    // The disk mappings are arguments Cubelet built on this node, not part
    // of the package: a malformed one or a missing private copy is infra.
    let map = stage("infra", parse_disk_map(&args.disks))?;
    let mut disks = rec.disks.clone().unwrap_or_default();
    for d in disks.iter_mut() {
        let Some(path) = d.path.clone() else {
            return Err(("config".into(), format!("disk {:?} has no path", d.id)));
        };
        match map.get(&path) {
            Some(copy) => d.path = Some(copy.clone()),
            None if d.readonly => {
                stage("deps", file_size(&path))?;
            }
            None => {
                return Err((
                    "config".into(),
                    format!(
                        "writable disk {} is not part of the package",
                        path.display()
                    ),
                ))
            }
        }
    }
    let pmem = rec.pmem.clone().unwrap_or_default();
    for p in &pmem {
        // A pmem without discard_writes is mapped shared and writable: the
        // trial guest could change a production file, and the checkpoint
        // does not carry its contents to another node.
        if !p.discard_writes {
            return Err((
                "config".into(),
                format!(
                    "pmem {} is writable, which cannot be restored elsewhere",
                    p.file.display()
                ),
            ));
        }
        let size = stage("deps", file_size(&p.file))?;
        if let Some(want) = p.size {
            if want != size {
                return Err((
                    "deps".into(),
                    format!(
                        "pmem {} is {} bytes, snapshot recorded {}",
                        p.file.display(),
                        size,
                        want
                    ),
                ));
            }
        }
    }
    let mut fss = rec.fs.clone().unwrap_or_default();
    for f in fss.iter_mut() {
        if let Some(b) = f.backendfs_config.as_mut() {
            b.shared_dir = args.share_dir.clone();
        }
    }
    Ok(RestoreConfig {
        source_url: PathBuf::from(Utils::get_snapshot_dir(&args.base, args.cpu, args.memory)),
        disks: Some(disks),
        net: rec.net.clone(),
        fs: Some(fss),
        pmem: Some(pmem),
        vsock: Some(Utils::gen_vsock_config(&args.vm_id)),
        memory_vol_url: Some(args.memory_vol.clone()),
        ..Default::default()
    })
}

/// The checks a production restore makes in `SandBox::restore_vm`, against
/// this node's read-only images.
fn check_compat(args: &VerifyRestoreArgs, rec: &ChVmConfig) -> CResult<()> {
    let ss_file = SnapshotInfo::load(&args.base, args.cpu, args.memory)?;
    let mut ss_req = SnapshotInfo::new(args.cpu, args.memory);
    let pmems = rec.pmem.clone().unwrap_or_default();
    let find = |id: &str| -> CResult<String> {
        pmems
            .iter()
            .find(|p| p.id.as_deref() == Some(id))
            .map(|p| p.file.to_string_lossy().to_string())
            .ok_or_else(|| format!("snapshot records no {} pmem", id))
    };
    ss_req.set_image_version_for_path(&find(HYP_OS_IMAGE_ID)?)?;
    ss_req.set_agent_version(&find(HYP_AGENT_ID)?)?;
    let kernel = rec
        .payload
        .as_ref()
        .and_then(|p| p.kernel.clone())
        .ok_or_else(|| "snapshot records no kernel".to_string())?;
    ss_req.set_kernel_version(&kernel.to_string_lossy())?;
    ss_req.vm_res = ss_file.vm_res.clone();
    ss_file
        .eq(&ss_req)
        .map_err(|e| format!("snapshot metadata not match:{}", e))
}

async fn check_agent(vm_id: &String, res: &mut VerifyResult) -> CResult<()> {
    let start = Instant::now();
    let mut last = String::new();
    while start.elapsed() < AGENT_BUDGET {
        res.agent_attempts += 1;
        let left = AGENT_BUDGET.saturating_sub(start.elapsed());
        let attempt = async {
            let conn = AsyncUtils::connect_agent(vm_id).await?;
            let client = agent_ttrpc::AgentServiceClient::new(conn);
            let ctx = context::with_timeout(AGENT_RPC_TIMEOUT_NS);
            let tm = Utc::now();
            let req = agent::SetGuestDateTimeRequest {
                Sec: tm.timestamp(),
                Usec: tm.timestamp_subsec_micros() as i64,
                ..Default::default()
            };
            client
                .set_guest_date_time(ctx.clone(), &req)
                .await
                .map_err(|e| format!("set guest time failed:{}", e))?;
            let req = agent::ReseedRandomDevRequest {
                data: Utils::get_rng()?,
                ..Default::default()
            };
            client
                .reseed_random_dev(ctx, &req)
                .await
                .map_err(|e| format!("reseed random failed:{}", e))?;
            Ok::<(), String>(())
        };
        match tokio::time::timeout(AGENT_ATTEMPT_TIMEOUT.min(left), attempt).await {
            Ok(Ok(())) => {
                res.agent_ms = start.elapsed().as_millis();
                return Ok(());
            }
            Ok(Err(e)) => last = e,
            Err(_) => last = "agent attempt timed out".to_string(),
        }
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
    Err(format!(
        "agent did not answer within {}s after {} attempts: {}",
        AGENT_BUDGET.as_secs(),
        res.agent_attempts,
        last
    ))
}

/// Classifies a failed VmRestore. A CPU feature mismatch is a property of
/// this host (the checkpoint may restore on a compatible one), and running
/// out of memory, file descriptors or space is a property of this moment.
/// Anything else is reported as a restore failure of the package.
fn restore_stage(err: &str) -> &'static str {
    let e = err.to_lowercase();
    if e.contains("cpu feature compatibility")
        || e.contains("cpuidcheckcompatibility")
        || e.contains("generating common cpuid")
    {
        return "compat";
    }
    const INFRA: [&str; 8] = [
        "cannot allocate memory",
        "os error 12",
        "resource temporarily unavailable",
        "os error 11",
        "no space left on device",
        "os error 28",
        "too many open files",
        "os error 24",
    ];
    if INFRA.iter().any(|p| e.contains(p)) {
        return "infra";
    }
    "restore"
}

fn wait_event(rx: &Receiver<NotifyEvent>, want: NotifyEvent, timeout: Duration) -> CResult<()> {
    let deadline = Instant::now() + timeout;
    loop {
        let left = deadline.saturating_duration_since(Instant::now());
        match rx.recv_timeout(left) {
            Ok(ev) if ev == want => return Ok(()),
            Ok(_) => continue,
            Err(e) => return Err(format!("waiting for {:?}: {}", want, e)),
        }
    }
}

async fn run(args: &VerifyRestoreArgs, res: &mut VerifyResult) -> StageResult<()> {
    check_vm_id(&args.vm_id)?;
    stage("isolation", check_isolation(res))?;

    let cfg_path = PathBuf::from(
        Utils::get_snapshot_dir(&args.base, args.cpu, args.memory).trim_start_matches("file://"),
    )
    .join("config.json");
    let raw = stage(
        "config",
        fs::read(&cfg_path).map_err(|e| format!("read {}: {}", cfg_path.display(), e)),
    )?;
    let rec: ChVmConfig = stage(
        "config",
        serde_json::from_slice(&raw).map_err(|e| format!("decode {}: {}", cfg_path.display(), e)),
    )?;
    stage("compat", check_compat(args, &rec))?;
    let config = restore_config(args, &rec)?;

    // The run directory holds the VMM API socket and the vsock socket; it is
    // removed when this guard drops.
    let _vm_dir = trial_run_dir(Path::new(VM_PATH), &args.vm_id)?;

    // Same syscall allowances the shim and `cube-runtime snapshot` grant
    // the in-process VMM.
    cube_hypervisor::set_runtime_seccomp_rules(vec![
        #[cfg(target_arch = "x86_64")]
        (libc::SYS_mkdir, vec![]),
        #[cfg(target_arch = "aarch64")]
        (libc::SYS_mkdirat, vec![]),
        (libc::SYS_getsockopt, vec![]),
        (libc::SYS_setsockopt, vec![]),
        (libc::SYS_faccessat2, vec![]),
    ]);
    let mut vmm_cfg = vmm_config::VmmConfig {
        sandbox_id: args.vm_id.clone(),
        ..Default::default()
    };
    let (tx, rx) = channel::<NotifyEvent>();
    vmm_cfg.event_notifier = Some(vmm_config::EventNotifyConfig { notifier: tx });
    let ch = stage(
        "infra",
        VmmInstance::new(vmm_cfg).map_err(|e| format!("new vmm instance failed:{}", e)),
    )?;

    let start = Instant::now();
    let restored = ch
        .send_request(ApiRequest::VmRestore(Arc::new(config)))
        .map_err(|e| format!("restore vm failed:{}", e))
        .and_then(|r| r.map_err(|e| format!("restore vm failed:{}", e)));
    res.restore_ms = start.elapsed().as_millis();
    let outcome = async {
        restored.map_err(|e| (restore_stage(&e).to_string(), e))?;
        // Like a production resume (the app-snapshot restore path in
        // SandBox::start_vm) this does not wait for the guest to announce
        // its vsock server: the announcement is part of the restored device
        // state, so the guest does not repeat it. The agent handshake below,
        // with the same retry budget, is the readiness check.
        if wait_event(&rx, NotifyEvent::VsockServerReady, Duration::from_millis(0)).is_ok() {
            res.vsock_ready_ms = start.elapsed().as_millis();
        }
        stage("agent", check_agent(&args.vm_id, res).await)
    }
    .await;

    // Tear the guest down before reporting; the process exit releases the
    // rest (memory mappings, the tap in the private namespace).
    let _ = ch.send_request(ApiRequest::VmShutdown);
    let _ = ch.send_request(ApiRequest::VmmShutdown);
    outcome
}

#[cfg(test)]
mod tests {
    use super::*;

    fn args(disks: Vec<String>) -> VerifyRestoreArgs {
        VerifyRestoreArgs {
            base: "/nonexistent-base".into(),
            vm_id: "ckpt-trial-test".into(),
            cpu: 2,
            memory: 1024,
            memory_vol: "file:///nonexistent-mem".into(),
            disks,
            share_dir: "/trial/share".into(),
        }
    }

    fn scratch(name: &str, len: usize) -> PathBuf {
        let dir = std::env::temp_dir().join(format!("verify-restore-test-{}", std::process::id()));
        fs::create_dir_all(&dir).unwrap();
        let p = dir.join(name);
        fs::write(&p, vec![0u8; len]).unwrap();
        p
    }

    fn config(json: serde_json::Value) -> ChVmConfig {
        serde_json::from_value(json).unwrap()
    }

    #[test]
    fn rewrites_disks_and_shares_to_private_paths() {
        let pmem = scratch("ro.img", 16);
        let copy = scratch("rootfs.copy", 8);
        let rec = config(serde_json::json!({
            "disks": [{"path": "/prod/sb-x-rootfs-gen0", "readonly": false}],
            "pmem": [{"file": pmem, "size": 16, "discard_writes": true}],
            "fs": [{"tag": "virtio_rw", "backendfs_config": {"shared_dir": "/prod/share"}}],
        }));
        let a = args(vec![format!("/prod/sb-x-rootfs-gen0={}", copy.display())]);
        let cfg = restore_config(&a, &rec).unwrap();
        assert_eq!(cfg.disks.unwrap()[0].path.as_deref(), Some(copy.as_path()));
        let fs = cfg.fs.unwrap();
        assert_eq!(
            fs[0].backendfs_config.as_ref().unwrap().shared_dir,
            "/trial/share"
        );
        assert_eq!(
            cfg.memory_vol_url.as_deref(),
            Some("file:///nonexistent-mem")
        );
    }

    #[test]
    fn refuses_a_writable_pmem() {
        let pmem = scratch("rw.img", 16);
        let rec = config(serde_json::json!({"pmem": [{"file": pmem, "discard_writes": false}]}));
        let err = restore_config(&args(vec![]), &rec).unwrap_err();
        assert_eq!(err.0, "config");
        assert!(err.1.contains("writable"), "{}", err.1);
    }

    #[test]
    fn refuses_an_unmapped_writable_disk() {
        let rec = config(serde_json::json!({
            "disks": [{"path": "/prod/extra-disk", "readonly": false}],
        }));
        let err = restore_config(&args(vec![]), &rec).unwrap_err();
        assert_eq!(err.0, "config");
    }

    #[test]
    fn refuses_ivshmem() {
        let rec = config(serde_json::json!({"ivshmem": {"path": "/dev/shm/x", "size": 1048576}}));
        let err = restore_config(&args(vec![]), &rec).unwrap_err();
        assert_eq!(err.0, "config");
    }

    #[test]
    fn reports_a_missing_private_disk_copy_as_infra() {
        let rec = config(serde_json::json!({
            "disks": [{"path": "/prod/sb-x-rootfs-gen0", "readonly": false}],
        }));
        let a = args(vec![
            "/prod/sb-x-rootfs-gen0=/nonexistent/rootfs.copy".into()
        ]);
        let err = restore_config(&a, &rec).unwrap_err();
        assert_eq!(err.0, "infra");
        assert!(err.1.contains("private disk copy"), "{}", err.1);
    }

    #[test]
    fn reports_a_malformed_disk_mapping_as_infra() {
        let rec = config(serde_json::json!({}));
        let err = restore_config(&args(vec!["no-mapping".into()]), &rec).unwrap_err();
        assert_eq!(err.0, "infra");
    }

    #[test]
    fn reports_an_invalid_vm_id_as_infra() {
        assert!(check_vm_id("ckpt-trial_01").is_ok());
        for bad in ["", "../escape", "trial id"] {
            assert_eq!(check_vm_id(bad).unwrap_err().0, "infra", "{:?}", bad);
        }
    }

    #[test]
    fn reports_a_run_dir_it_cannot_create_as_infra() {
        // A file where the run root should be: creating the directory fails.
        let root = scratch("not-a-dir", 1);
        match trial_run_dir(&root, "ckpt-trial-test") {
            Ok(_) => panic!("a run directory under a file must not be created"),
            Err(err) => assert_eq!(err.0, "infra"),
        }
        let ok_root = std::env::temp_dir().join(format!("verify-run-{}", std::process::id()));
        let dir = ok_root.join("ckpt-trial-test");
        {
            let _guard = trial_run_dir(&ok_root, "ckpt-trial-test").unwrap();
            assert!(dir.is_dir());
        }
        assert!(!dir.exists(), "the run directory goes with its guard");
    }

    #[test]
    fn classifies_restore_failures() {
        assert_eq!(
            restore_stage("restore vm failed:Error checking cpu feature compatibility': CpuidCheckCompatibility"),
            "compat"
        );
        assert_eq!(
            restore_stage("restore vm failed: Cannot allocate memory (os error 12)"),
            "infra"
        );
        assert_eq!(
            restore_stage("restore vm failed: No space left on device (os error 28)"),
            "infra"
        );
        assert_eq!(
            restore_stage("restore vm failed: invalid snapshot state"),
            "restore"
        );
    }

    #[test]
    fn reports_a_missing_read_only_image_as_a_dependency() {
        let rec = config(serde_json::json!({
            "pmem": [{"file": "/nonexistent/image.ext4", "discard_writes": true}],
        }));
        let err = restore_config(&args(vec![]), &rec).unwrap_err();
        assert_eq!(err.0, "deps");
    }
}
