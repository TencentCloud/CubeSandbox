// Copyright (c) 2019 Ant Financial
//
// SPDX-License-Identifier: Apache-2.0
//

use nix::sys::stat;
use regex::Regex;
use std::collections::HashMap;
use std::ffi::OsStr;
use std::fmt;
use std::fs;
use std::os::unix::ffi::OsStrExt;
use std::os::unix::fs::MetadataExt;
use std::path::{Path, PathBuf};
use std::str::FromStr;
use std::sync::Arc;
use tokio::sync::Mutex;

use crate::linux_abi::*;
use crate::pci;
use crate::sandbox::Sandbox;
use crate::uevent::{wait_for_uevent, Uevent, UeventMatcher};
use anyhow::{anyhow, Context, Result};
use oci::{LinuxDeviceCgroup, LinuxResources, Spec};
use protocols::agent::Device;
use tracing::instrument;

// Convenience macro to obtain the scope logger
macro_rules! sl {
    () => {
        slog_scope::logger().new(o!("subsystem" => "device"))
    };
}

const VM_ROOTFS: &str = "/";

pub const DRIVER_VIRTIOFS_TYPE: &str = "virtio-fs";
pub const DRIVER_BLK_TYPE: &str = "blk";
pub const DRIVER_BLK_CUBE_TYPE: &str = "blk-cube";
pub const DRIVER_CUBE_BIND_TYPE: &str = "bind-cube";
pub const DRIVER_MMIO_BLK_TYPE: &str = "mmioblk";
pub const DRIVER_SCSI_TYPE: &str = "scsi";
pub const DRIVER_NVDIMM_TYPE: &str = "nvdimm";
pub const DRIVER_EPHEMERAL_TYPE: &str = "ephemeral";
pub const DRIVER_LOCAL_TYPE: &str = "local";
pub const DRIVER_BIND_SHARE_TYPE: &str = "bind-share";
pub const DRIVER_WATCHABLE_BIND_TYPE: &str = "watchable-bind";
// VFIO device to be bound to a guest kernel driver
pub const DRIVER_VFIO_GK_TYPE: &str = "vfio-gk";
// VFIO device to be bound to vfio-pci and made available inside the
// container as a VFIO device node
pub const DRIVER_VFIO_TYPE: &str = "vfio";
pub const DRIVER_OVERLAYFS_TYPE: &str = "overlayfs";
pub const FS_TYPE_HUGETLB: &str = "hugetlbfs";

#[derive(Debug)]
struct CubeVirtioBlkPciMatcher {
    prefix: String,
}

impl CubeVirtioBlkPciMatcher {
    fn new(p: &str) -> CubeVirtioBlkPciMatcher {
        CubeVirtioBlkPciMatcher {
            prefix: p.to_string(),
        }
    }
}

impl UeventMatcher for CubeVirtioBlkPciMatcher {
    fn is_match(&self, uev: &Uevent) -> bool {
        uev.subsystem == "block" && uev.devpath.starts_with(&self.prefix) && !uev.devname.is_empty()
    }
}

#[instrument]
pub fn online_device(path: &str) -> Result<()> {
    fs::write(path, "1")?;
    Ok(())
}

// Force a given PCI device to bind to the given driver, does
// basically the same thing as
//    driverctl set-override <PCI address> <driver>
#[instrument]
pub fn pci_driver_override<T, U>(syspci: T, dev: pci::Address, drv: U) -> Result<()>
where
    T: AsRef<OsStr> + std::fmt::Debug,
    U: AsRef<OsStr> + std::fmt::Debug,
{
    let syspci = Path::new(&syspci);
    let drv = drv.as_ref();
    info!(sl!(), "rebind_pci_driver: {} => {:?}", dev, drv);

    let devpath = syspci.join("devices").join(dev.to_string());
    let overridepath = &devpath.join("driver_override");

    fs::write(overridepath, drv.as_bytes())?;

    let drvpath = &devpath.join("driver");
    let need_unbind = match fs::read_link(drvpath) {
        Ok(d) if d.file_name() == Some(drv) => return Ok(()), // Nothing to do
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => false, // No current driver
        Err(e) => return Err(anyhow!("Error checking driver on {}: {}", dev, e)),
        Ok(_) => true, // Current driver needs unbinding
    };
    if need_unbind {
        let unbindpath = &drvpath.join("unbind");
        fs::write(unbindpath, dev.to_string())?;
    }
    let probepath = syspci.join("drivers_probe");
    fs::write(probepath, dev.to_string())?;
    Ok(())
}

// Represents an IOMMU group
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct IommuGroup(u32);

impl fmt::Display for IommuGroup {
    fn fmt(&self, f: &mut fmt::Formatter) -> Result<(), fmt::Error> {
        write!(f, "{}", self.0)
    }
}

// Determine the IOMMU group of a PCI device
#[instrument]
fn pci_iommu_group<T>(syspci: T, dev: pci::Address) -> Result<Option<IommuGroup>>
where
    T: AsRef<OsStr> + std::fmt::Debug,
{
    let syspci = Path::new(&syspci);
    let grouppath = syspci
        .join("devices")
        .join(dev.to_string())
        .join("iommu_group");

    match fs::read_link(&grouppath) {
        // Device has no group
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(None),
        Err(e) => Err(anyhow!("Error reading link {:?}: {}", &grouppath, e)),
        Ok(group) => {
            if let Some(group) = group.file_name() {
                if let Some(group) = group.to_str() {
                    if let Ok(group) = group.parse::<u32>() {
                        return Ok(Some(IommuGroup(group)));
                    }
                }
            }
            Err(anyhow!(
                "Unexpected IOMMU group link {:?} => {:?}",
                grouppath,
                group
            ))
        }
    }
}

// pcipath_to_sysfs fetches the sysfs path for a PCI path, relative to
// the sysfs path for the PCI host bridge, based on the PCI path
// provided.
#[instrument]
pub fn pcipath_to_sysfs(root_bus_sysfs: &str, pcipath: &pci::Path) -> Result<String> {
    let mut bus = "0000:00".to_string();
    let mut relpath = String::new();

    for i in 0..pcipath.len() {
        let bdf = format!("{}:{}", bus, pcipath[i]);

        relpath = format!("{}/{}", relpath, bdf);

        if i == pcipath.len() - 1 {
            // Final device need not be a bridge
            break;
        }

        // Find out the bus exposed by bridge
        let bridgebuspath = format!("{}{}/pci_bus", root_bus_sysfs, relpath);
        let mut files: Vec<_> = fs::read_dir(&bridgebuspath)?.collect();

        match files.pop() {
            Some(busfile) if files.is_empty() => {
                bus = busfile?
                    .file_name()
                    .into_string()
                    .map_err(|e| anyhow!("Bad filename under {}: {:?}", &bridgebuspath, e))?;
            }
            _ => {
                return Err(anyhow!(
                    "Expected exactly one PCI bus in {}, got {} instead",
                    bridgebuspath,
                    // Adjust to original value as we've already popped
                    files.len() + 1
                ));
            }
        };
    }

    Ok(relpath)
}

// FIXME: This matcher is only correct if the guest has at most one
// SCSI host.
#[derive(Debug)]
struct ScsiBlockMatcher {
    search: String,
}

impl ScsiBlockMatcher {
    fn new(scsi_addr: &str) -> ScsiBlockMatcher {
        let search = format!(r"/0:0:{}/block/", scsi_addr);

        ScsiBlockMatcher { search }
    }
}

impl UeventMatcher for ScsiBlockMatcher {
    fn is_match(&self, uev: &Uevent) -> bool {
        uev.subsystem == "block" && uev.devpath.contains(&self.search) && !uev.devname.is_empty()
    }
}

#[instrument]
pub async fn get_scsi_device_name(
    sandbox: &Arc<Mutex<Sandbox>>,
    scsi_addr: &str,
) -> Result<String> {
    let matcher = ScsiBlockMatcher::new(scsi_addr);

    scan_scsi_bus(scsi_addr)?;
    let uev = wait_for_uevent(sandbox, matcher).await?;
    Ok(format!("{}/{}", SYSTEM_DEV_PATH, &uev.devname))
}

#[derive(Debug)]
struct VirtioBlkPciMatcher {
    rex: Regex,
}

impl VirtioBlkPciMatcher {
    fn new(relpath: &str) -> VirtioBlkPciMatcher {
        let root_bus = create_pci_root_bus_path();
        let re = format!(r"^{}{}/virtio[0-9]+/block/", root_bus, relpath);

        VirtioBlkPciMatcher {
            rex: Regex::new(&re).expect("BUG: failed to compile VirtioBlkPciMatcher regex"),
        }
    }
}

impl UeventMatcher for VirtioBlkPciMatcher {
    fn is_match(&self, uev: &Uevent) -> bool {
        uev.subsystem == "block" && self.rex.is_match(&uev.devpath) && !uev.devname.is_empty()
    }
}

#[instrument]
pub async fn get_device_name_by_pci(
    sandbox: &Arc<Mutex<Sandbox>>,
    pcipath: &str,
) -> Result<String> {
    let pci_prefix = format!("{}/{}", create_pci_root_bus_path(), pcipath);
    let matcher = CubeVirtioBlkPciMatcher::new(pci_prefix.as_str());
    let uev = wait_for_uevent(sandbox, matcher).await?;
    Ok(format!("{}/{}", SYSTEM_DEV_PATH, &uev.devname))
}

#[instrument]
pub async fn get_virtio_blk_pci_device_name(
    sandbox: &Arc<Mutex<Sandbox>>,
    pcipath: &pci::Path,
) -> Result<String> {
    let root_bus_sysfs = format!("{}{}", SYSFS_DIR, create_pci_root_bus_path());
    let sysfs_rel_path = pcipath_to_sysfs(&root_bus_sysfs, pcipath)?;
    let matcher = VirtioBlkPciMatcher::new(&sysfs_rel_path);

    let uev = wait_for_uevent(sandbox, matcher).await?;
    Ok(format!("{}/{}", SYSTEM_DEV_PATH, &uev.devname))
}

#[derive(Debug)]
struct PmemBlockMatcher {
    dev_name: String,
}

impl PmemBlockMatcher {
    fn new(name: &str) -> PmemBlockMatcher {
        PmemBlockMatcher {
            dev_name: name.to_string(),
        }
    }
}

impl UeventMatcher for PmemBlockMatcher {
    fn is_match(&self, uev: &Uevent) -> bool {
        uev.subsystem == "block" && uev.devname == self.dev_name
    }
}

#[instrument]
pub async fn wait_for_pmem_device(sandbox: &Arc<Mutex<Sandbox>>, devpath: &str) -> Result<()> {
    let devname = match devpath.strip_prefix("/dev/") {
        Some(dev) => dev,
        None => {
            return Err(anyhow!(
                "Storage source '{}' must start with /dev/",
                devpath
            ))
        }
    };

    let matcher = PmemBlockMatcher::new(devname);
    let _ = wait_for_uevent(sandbox, matcher).await?;
    Ok(())
}

#[derive(Debug)]
struct PciMatcher {
    devpath: String,
}

impl PciMatcher {
    fn new(relpath: &str) -> Result<PciMatcher> {
        let root_bus = create_pci_root_bus_path();
        Ok(PciMatcher {
            devpath: format!("{}{}", root_bus, relpath),
        })
    }
}

impl UeventMatcher for PciMatcher {
    fn is_match(&self, uev: &Uevent) -> bool {
        uev.devpath == self.devpath
    }
}

pub async fn wait_for_pci_device(
    sandbox: &Arc<Mutex<Sandbox>>,
    pcipath: &pci::Path,
) -> Result<pci::Address> {
    let root_bus_sysfs = format!("{}{}", SYSFS_DIR, create_pci_root_bus_path());
    let sysfs_rel_path = pcipath_to_sysfs(&root_bus_sysfs, pcipath)?;
    let matcher = PciMatcher::new(&sysfs_rel_path)?;

    let uev = wait_for_uevent(sandbox, matcher).await?;

    let addr = uev
        .devpath
        .rsplit('/')
        .next()
        .ok_or_else(|| anyhow!("Bad device path {:?} in uevent", &uev.devpath))?;
    let addr = pci::Address::from_str(addr)?;
    Ok(addr)
}

pub async fn wait_for_pci_net(
    sandbox: &Arc<Mutex<Sandbox>>,
    pcipath: &pci::Path,
) -> Result<String> {
    let root_bus_sysfs = format!("{}{}", SYSFS_DIR, create_pci_root_bus_path());
    let sysfs_rel_path = pcipath_to_sysfs(&root_bus_sysfs, pcipath)?;
    let matcher = PciNetMatcher::new(&sysfs_rel_path)?;

    let uev = wait_for_uevent(sandbox, matcher).await?;

    let addr = uev
        .devpath
        .rsplit('/')
        .next()
        .ok_or_else(|| anyhow!("Bad device path {:?} in uevent", &uev.devpath))?;
    Ok(String::from_str(addr)?)
}

#[derive(Debug)]
struct PciNetMatcher {
    devpath: String,
}
impl PciNetMatcher {
    fn new(relpath: &str) -> Result<PciNetMatcher> {
        let root_bus = create_pci_root_bus_path();
        Ok(PciNetMatcher {
            devpath: format!("{}{}", root_bus, relpath),
        })
    }
}

impl UeventMatcher for PciNetMatcher {
    fn is_match(&self, uev: &Uevent) -> bool {
        uev.subsystem == "net" && uev.devpath.starts_with(&self.devpath)
    }
}

#[derive(Debug)]
struct VfioMatcher {
    syspath: String,
}

impl VfioMatcher {
    fn new(grp: IommuGroup) -> VfioMatcher {
        VfioMatcher {
            syspath: format!("/devices/virtual/vfio/{}", grp),
        }
    }
}

impl UeventMatcher for VfioMatcher {
    fn is_match(&self, uev: &Uevent) -> bool {
        uev.devpath == self.syspath
    }
}

#[instrument]
async fn get_vfio_device_name(sandbox: &Arc<Mutex<Sandbox>>, grp: IommuGroup) -> Result<String> {
    let matcher = VfioMatcher::new(grp);

    let uev = wait_for_uevent(sandbox, matcher).await?;
    Ok(format!("{}/{}", SYSTEM_DEV_PATH, &uev.devname))
}

/// Scan SCSI bus for the given SCSI address(SCSI-Id and LUN)
#[instrument]
fn scan_scsi_bus(scsi_addr: &str) -> Result<()> {
    let tokens: Vec<&str> = scsi_addr.split(':').collect();
    if tokens.len() != 2 {
        return Err(anyhow!(
            "Unexpected format for SCSI Address: {}, expect SCSIID:LUA",
            scsi_addr
        ));
    }

    // Scan scsi host passing in the channel, SCSI id and LUN.
    // Channel is always 0 because we have only one SCSI controller.
    let scan_data = format!("0 {} {}", tokens[0], tokens[1]);

    for entry in fs::read_dir(SYSFS_SCSI_HOST_PATH)? {
        let host = entry?.file_name();

        let host_str = host.to_str().ok_or_else(|| {
            anyhow!(
                "failed to convert directory entry to unicode for file {:?}",
                host
            )
        })?;

        let scan_path = PathBuf::from(&format!("{}/{}/{}", SYSFS_SCSI_HOST_PATH, host_str, "scan"));

        fs::write(scan_path, &scan_data)?;
    }

    Ok(())
}

#[derive(Debug, Clone)]
struct DevNumUpdate {
    // the major and minor numbers for the device within the guest
    guest_major: i64,
    guest_minor: i64,
}

impl DevNumUpdate {
    fn from_vm_path<T: AsRef<Path>>(vm_path: T) -> Result<Self> {
        let vm_path = vm_path.as_ref();

        if !vm_path.exists() {
            return Err(anyhow!("VM device path {:?} doesn't exist", vm_path));
        }

        let devid = fs::metadata(vm_path)?.rdev();
        let guest_major = stat::major(devid) as i64;
        let guest_minor = stat::minor(devid) as i64;

        Ok(DevNumUpdate {
            guest_major,
            guest_minor,
        })
    }
}

// Represents the device-node and resource related updates to the OCI
// spec needed for a particular device
#[derive(Debug, Clone)]
struct DevUpdate {
    num: DevNumUpdate,
    // an optional new path to update the device to in the "inner" container
    // specification
    final_path: Option<String>,
}

impl DevUpdate {
    fn from_vm_path<T: AsRef<Path>>(vm_path: T, final_path: String) -> Result<Self> {
        Ok(DevUpdate {
            final_path: Some(final_path),
            ..DevNumUpdate::from_vm_path(vm_path)?.into()
        })
    }
}

impl From<DevNumUpdate> for DevUpdate {
    fn from(num: DevNumUpdate) -> Self {
        DevUpdate {
            num,
            final_path: None,
        }
    }
}

// Represents the updates to the OCI spec needed for a particular device
#[derive(Debug, Clone, Default)]
struct SpecUpdate {
    dev: Option<DevUpdate>,
    // optional corrections for PCI addresses
    pci: Vec<(pci::Address, pci::Address)>,
}

impl<T: Into<DevUpdate>> From<T> for SpecUpdate {
    fn from(dev: T) -> Self {
        SpecUpdate {
            dev: Some(dev.into()),
            pci: Vec::new(),
        }
    }
}

// update_spec_devices updates the device list in the OCI spec to make
// it include details appropriate for the VM, instead of the host.  It
// is given a map of (container_path => update) where:
//     container_path: the path to the device in the original OCI spec
//     update: information on changes to make to the device
#[instrument]
fn update_spec_devices(spec: &mut Spec, mut updates: HashMap<&str, DevUpdate>) -> Result<()> {
    let linux = spec
        .linux
        .as_mut()
        .ok_or_else(|| anyhow!("Spec didn't contain linux field"))?;
    let mut res_updates = HashMap::<(&str, i64, i64), DevNumUpdate>::with_capacity(updates.len());

    for specdev in &mut linux.devices {
        if let Some(update) = updates.remove(specdev.path.as_str()) {
            let host_major = specdev.major;
            let host_minor = specdev.minor;

            info!(
                sl!(),
                "update_spec_devices() updating device";
                "container_path" => &specdev.path,
                "type" => &specdev.r#type,
                "host_major" => host_major,
                "host_minor" => host_minor,
                "guest_major" => update.num.guest_major,
                "guest_minor" => update.num.guest_minor,
                "final_path" => update.final_path.as_ref(),
            );

            specdev.major = update.num.guest_major;
            specdev.minor = update.num.guest_minor;
            if let Some(final_path) = update.final_path {
                specdev.path = final_path;
            }

            if res_updates
                .insert(
                    (specdev.r#type.as_str(), host_major, host_minor),
                    update.num,
                )
                .is_some()
            {
                return Err(anyhow!(
                    "Conflicting resource updates for host_major={} host_minor={}",
                    host_major,
                    host_minor
                ));
            }
        }
    }

    // Make sure we applied all of our updates
    if !updates.is_empty() {
        return Err(anyhow!(
            "Missing devices in OCI spec: {:?}",
            updates
                .keys()
                .map(|d| format!("{:?}", d))
                .collect::<Vec<_>>()
                .join(" ")
        ));
    }

    if let Some(resources) = linux.resources.as_mut() {
        for r in &mut resources.devices {
            if let (Some(host_major), Some(host_minor)) = (r.major, r.minor) {
                if let Some(update) = res_updates.get(&(r.r#type.as_str(), host_major, host_minor))
                {
                    info!(
                        sl!(),
                        "update_spec_devices() updating resource";
                        "type" => &r.r#type,
                        "host_major" => host_major,
                        "host_minor" => host_minor,
                        "guest_major" => update.guest_major,
                        "guest_minor" => update.guest_minor,
                    );

                    r.major = Some(update.guest_major);
                    r.minor = Some(update.guest_minor);
                }
            }
        }
    }

    Ok(())
}

// update_env_pci alters PCI addresses in a set of environment
// variables to be correct for the VM instead of the host.  It is
// given a map of (host address => guest address)
#[instrument]
pub fn update_env_pci(
    env: &mut [String],
    pcimap: &HashMap<pci::Address, pci::Address>,
) -> Result<()> {
    for envvar in env {
        let eqpos = envvar
            .find('=')
            .ok_or_else(|| anyhow!("Malformed OCI env entry {:?}", envvar))?;

        let (name, eqval) = envvar.split_at(eqpos);
        let val = &eqval[1..];

        if !name.starts_with("PCIDEVICE_") {
            continue;
        }

        let mut guest_addrs = Vec::<String>::new();

        for host_addr in val.split(',') {
            let host_addr = pci::Address::from_str(host_addr)
                .with_context(|| format!("Can't parse {} environment variable", name))?;
            let guest_addr = pcimap
                .get(&host_addr)
                .ok_or_else(|| anyhow!("Unable to translate host PCI address {}", host_addr))?;
            guest_addrs.push(format!("{}", guest_addr));
        }

        envvar.replace_range(eqpos + 1.., guest_addrs.join(",").as_str());
    }

    Ok(())
}

// device.Id should be the predicted device name (vda, vdb, ...)
// device.VmPath already provides a way to send it in
#[instrument]
async fn virtiommio_blk_device_handler(
    device: &Device,
    _sandbox: &Arc<Mutex<Sandbox>>,
) -> Result<SpecUpdate> {
    if device.vm_path.is_empty() {
        return Err(anyhow!("Invalid path for virtio mmio blk device"));
    }

    Ok(DevNumUpdate::from_vm_path(&device.vm_path)?.into())
}

// device.Id should be a PCI path string
#[instrument]
async fn virtio_blk_device_handler(
    device: &Device,
    sandbox: &Arc<Mutex<Sandbox>>,
) -> Result<SpecUpdate> {
    let pcipath = pci::Path::from_str(&device.id)?;
    let vm_path = get_virtio_blk_pci_device_name(sandbox, &pcipath).await?;

    Ok(DevNumUpdate::from_vm_path(vm_path)?.into())
}

// device.Id should be the SCSI address of the disk in the format "scsiID:lunID"
#[instrument]
async fn virtio_scsi_device_handler(
    device: &Device,
    sandbox: &Arc<Mutex<Sandbox>>,
) -> Result<SpecUpdate> {
    let vm_path = get_scsi_device_name(sandbox, &device.id).await?;

    Ok(DevNumUpdate::from_vm_path(vm_path)?.into())
}

#[instrument]
async fn virtio_nvdimm_device_handler(
    device: &Device,
    _sandbox: &Arc<Mutex<Sandbox>>,
) -> Result<SpecUpdate> {
    if device.vm_path.is_empty() {
        return Err(anyhow!("Invalid path for nvdimm device"));
    }

    Ok(DevNumUpdate::from_vm_path(&device.vm_path)?.into())
}

fn split_vfio_option(opt: &str) -> Option<(&str, &str)> {
    let mut tokens = opt.split('=');
    let hostbdf = tokens.next()?;
    let path = tokens.next()?;
    if tokens.next().is_some() {
        None
    } else {
        Some((hostbdf, path))
    }
}

// device.options should have one entry for each PCI device in the VFIO group
// Each option should have the form "DDDD:BB:DD.F=<pcipath>"
//     DDDD:BB:DD.F is the device's PCI address in the host
//     <pcipath> is a PCI path to the device in the guest (see pci.rs)
async fn vfio_device_handler(device: &Device, sandbox: &Arc<Mutex<Sandbox>>) -> Result<SpecUpdate> {
    let vfio_in_guest = device.type_ != DRIVER_VFIO_GK_TYPE;
    let mut pci_fixups = Vec::<(pci::Address, pci::Address)>::new();
    let mut group = None;

    for opt in device.options.iter() {
        let (host, pcipath) =
            split_vfio_option(opt).ok_or_else(|| anyhow!("Malformed VFIO option {:?}", opt))?;
        let host =
            pci::Address::from_str(host).context("Bad host PCI address in VFIO option {:?}")?;
        let pcipath = pci::Path::from_str(pcipath)?;

        let guestdev = wait_for_pci_device(sandbox, &pcipath).await?;
        if vfio_in_guest {
            pci_driver_override(SYSFS_BUS_PCI_PATH, guestdev, "vfio-pci")?;

            // Devices must have an IOMMU group to be usable via VFIO
            let devgroup = pci_iommu_group(SYSFS_BUS_PCI_PATH, guestdev)?
                .ok_or_else(|| anyhow!("{} has no IOMMU group", guestdev))?;

            if let Some(g) = group {
                if g != devgroup {
                    return Err(anyhow!("{} is not in guest IOMMU group {}", guestdev, g));
                }
            }

            group = Some(devgroup);

            pci_fixups.push((host, guestdev));
        }
    }

    let dev_update = if vfio_in_guest {
        // If there are any devices at all, logic above ensures that group is not None
        let group = group.ok_or_else(|| anyhow!("failed to get VFIO group"))?;

        let vm_path = get_vfio_device_name(sandbox, group).await?;

        Some(DevUpdate::from_vm_path(&vm_path, vm_path.clone())?)
    } else {
        None
    };

    Ok(SpecUpdate {
        dev: dev_update,
        pci: pci_fixups,
    })
}

#[instrument]
pub async fn add_devices(
    devices: &[Device],
    spec: &mut Spec,
    sandbox: &Arc<Mutex<Sandbox>>,
) -> Result<()> {
    let mut dev_updates = HashMap::<&str, DevUpdate>::with_capacity(devices.len());

    for device in devices.iter() {
        let update = add_device(device, sandbox).await?;
        if let Some(dev_update) = update.dev {
            if dev_updates
                .insert(&device.container_path, dev_update)
                .is_some()
            {
                return Err(anyhow!(
                    "Conflicting device updates for {}",
                    &device.container_path
                ));
            }

            let mut sb = sandbox.lock().await;
            for (host, guest) in update.pci {
                if let Some(other_guest) = sb.pcimap.insert(host, guest) {
                    return Err(anyhow!(
                        "Conflicting guest address for host device {} ({} versus {})",
                        host,
                        guest,
                        other_guest
                    ));
                }
            }
        }
    }

    if let Some(process) = spec.process.as_mut() {
        update_env_pci(&mut process.env, &sandbox.lock().await.pcimap)?
    }
    update_spec_devices(spec, dev_updates)
}

#[instrument]
async fn add_device(device: &Device, sandbox: &Arc<Mutex<Sandbox>>) -> Result<SpecUpdate> {
    // log before validation to help with debugging gRPC protocol version differences.
    info!(sl!(), "device-id: {}, device-type: {}, device-vm-path: {}, device-container-path: {}, device-options: {:?}",
          device.id, device.type_, device.vm_path, device.container_path, device.options);

    if device.type_.is_empty() {
        return Err(anyhow!("invalid type for device {:?}", device));
    }

    if device.id.is_empty() && device.vm_path.is_empty() {
        return Err(anyhow!("invalid ID and VM path for device {:?}", device));
    }

    if device.container_path.is_empty() {
        return Err(anyhow!("invalid container path for device {:?}", device));
    }

    match device.type_.as_str() {
        DRIVER_BLK_TYPE => virtio_blk_device_handler(device, sandbox).await,
        DRIVER_MMIO_BLK_TYPE => virtiommio_blk_device_handler(device, sandbox).await,
        DRIVER_NVDIMM_TYPE => virtio_nvdimm_device_handler(device, sandbox).await,
        DRIVER_SCSI_TYPE => virtio_scsi_device_handler(device, sandbox).await,
        DRIVER_VFIO_GK_TYPE | DRIVER_VFIO_TYPE => vfio_device_handler(device, sandbox).await,
        _ => Err(anyhow!("Unknown device type {}", device.type_)),
    }
}

// update_device_cgroup update the device cgroup for container
// to not allow access to the guest root partition. This prevents
// the container from being able to access the VM rootfs.
#[instrument]
pub fn update_device_cgroup(spec: &mut Spec) -> Result<()> {
    let meta = fs::metadata(VM_ROOTFS)?;
    let rdev = meta.dev();
    let major = stat::major(rdev) as i64;
    let minor = stat::minor(rdev) as i64;

    let linux = spec
        .linux
        .as_mut()
        .ok_or_else(|| anyhow!("Spec didn't container linux field"))?;

    let resources = linux.resources.get_or_insert(LinuxResources::default());

    resources.devices.push(LinuxDeviceCgroup {
        allow: false,
        major: Some(major),
        minor: Some(minor),
        r#type: String::from("b"),
        access: String::from("rw"),
    });

    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::uevent::spawn_test_watcher;
    use oci::Linux;
    use std::iter::FromIterator;
    use tempfile::tempdir;

    #[test]
    fn test_update_device_cgroup() {
        let mut spec = Spec {
            linux: Some(Linux::default()),
            ..Default::default()
        };

        update_device_cgroup(&mut spec).unwrap();

        let devices = spec.linux.unwrap().resources.unwrap().devices;
        assert_eq!(devices.len(), 1);

        let meta = fs::metadata(VM_ROOTFS).unwrap();
        let rdev = meta.dev();
        let major = stat::major(rdev) as i64;
        let minor = stat::minor(rdev) as i64;

        assert_eq!(devices[0].major, Some(major));
        assert_eq!(devices[0].minor, Some(minor));
    }

    #[test]
    fn test_update_spec_devices() {
        let (major, minor) = (7, 2);
        let mut spec = Spec::default();

        // vm_path empty
        let update = DevNumUpdate::from_vm_path("");
        assert!(update.is_err());

        // linux is empty
        let container_path = "/dev/null";
        let vm_path = "/dev/null";
        let res = update_spec_devices(
            &mut spec,
            HashMap::from_iter(vec![(
                container_path,
                DevNumUpdate::from_vm_path(vm_path).unwrap().into(),
            )]),
        );
        assert!(res.is_err());

        spec.linux = Some(Linux::default());

        // linux.devices doesn't contain the updated device
        let res = update_spec_devices(
            &mut spec,
            HashMap::from_iter(vec![(
                container_path,
                DevNumUpdate::from_vm_path(vm_path).unwrap().into(),
            )]),
        );
        assert!(res.is_err());

        spec.linux.as_mut().unwrap().devices = vec![oci::LinuxDevice {
            path: "/dev/null2".to_string(),
            major,
            minor,
            ..oci::LinuxDevice::default()
        }];

        // guest and host path are not the same
        let res = update_spec_devices(
            &mut spec,
            HashMap::from_iter(vec![(
                container_path,
                DevNumUpdate::from_vm_path(vm_path).unwrap().into(),
            )]),
        );
        assert!(
            res.is_err(),
            "container_path={:?} vm_path={:?} spec={:?}",
            container_path,
            vm_path,
            spec
        );

        spec.linux.as_mut().unwrap().devices[0].path = container_path.to_string();

        // spec.linux.resources is empty
        let res = update_spec_devices(
            &mut spec,
            HashMap::from_iter(vec![(
                container_path,
                DevNumUpdate::from_vm_path(vm_path).unwrap().into(),
            )]),
        );
        assert!(res.is_ok());

        // update both devices and cgroup lists
        spec.linux.as_mut().unwrap().devices = vec![oci::LinuxDevice {
            path: container_path.to_string(),
            major,
            minor,
            ..oci::LinuxDevice::default()
        }];

        spec.linux.as_mut().unwrap().resources = Some(oci::LinuxResources {
            devices: vec![oci::LinuxDeviceCgroup {
                major: Some(major),
                minor: Some(minor),
                ..oci::LinuxDeviceCgroup::default()
            }],
            ..oci::LinuxResources::default()
        });

        let res = update_spec_devices(
            &mut spec,
            HashMap::from_iter(vec![(
                container_path,
                DevNumUpdate::from_vm_path(vm_path).unwrap().into(),
            )]),
        );
        assert!(res.is_ok());
    }

    #[test]
    fn test_update_spec_devices_guest_host_conflict() {
        let null_rdev = fs::metadata("/dev/null").unwrap().rdev();
        let zero_rdev = fs::metadata("/dev/zero").unwrap().rdev();
        let full_rdev = fs::metadata("/dev/full").unwrap().rdev();

        let host_major_a = stat::major(null_rdev) as i64;
        let host_minor_a = stat::minor(null_rdev) as i64;
        let host_major_b = stat::major(zero_rdev) as i64;
        let host_minor_b = stat::minor(zero_rdev) as i64;

        let mut spec = Spec {
            linux: Some(Linux {
                devices: vec![
                    oci::LinuxDevice {
                        path: "/dev/a".to_string(),
                        r#type: "c".to_string(),
                        major: host_major_a,
                        minor: host_minor_a,
                        ..oci::LinuxDevice::default()
                    },
                    oci::LinuxDevice {
                        path: "/dev/b".to_string(),
                        r#type: "c".to_string(),
                        major: host_major_b,
                        minor: host_minor_b,
                        ..oci::LinuxDevice::default()
                    },
                ],
                resources: Some(LinuxResources {
                    devices: vec![
                        oci::LinuxDeviceCgroup {
                            r#type: "c".to_string(),
                            major: Some(host_major_a),
                            minor: Some(host_minor_a),
                            ..oci::LinuxDeviceCgroup::default()
                        },
                        oci::LinuxDeviceCgroup {
                            r#type: "c".to_string(),
                            major: Some(host_major_b),
                            minor: Some(host_minor_b),
                            ..oci::LinuxDeviceCgroup::default()
                        },
                    ],
                    ..LinuxResources::default()
                }),
                ..Linux::default()
            }),
            ..Spec::default()
        };

        let container_path_a = "/dev/a";
        let vm_path_a = "/dev/zero";

        let guest_major_a = stat::major(zero_rdev) as i64;
        let guest_minor_a = stat::minor(zero_rdev) as i64;

        let container_path_b = "/dev/b";
        let vm_path_b = "/dev/full";

        let guest_major_b = stat::major(full_rdev) as i64;
        let guest_minor_b = stat::minor(full_rdev) as i64;

        let specdevices = &spec.linux.as_ref().unwrap().devices;
        assert_eq!(host_major_a, specdevices[0].major);
        assert_eq!(host_minor_a, specdevices[0].minor);
        assert_eq!(host_major_b, specdevices[1].major);
        assert_eq!(host_minor_b, specdevices[1].minor);

        let specresources = spec.linux.as_ref().unwrap().resources.as_ref().unwrap();
        assert_eq!(Some(host_major_a), specresources.devices[0].major);
        assert_eq!(Some(host_minor_a), specresources.devices[0].minor);
        assert_eq!(Some(host_major_b), specresources.devices[1].major);
        assert_eq!(Some(host_minor_b), specresources.devices[1].minor);

        let updates = HashMap::from_iter(vec![
            (
                container_path_a,
                DevNumUpdate::from_vm_path(vm_path_a).unwrap().into(),
            ),
            (
                container_path_b,
                DevNumUpdate::from_vm_path(vm_path_b).unwrap().into(),
            ),
        ]);
        let res = update_spec_devices(&mut spec, updates);
        assert!(res.is_ok());

        let specdevices = &spec.linux.as_ref().unwrap().devices;
        assert_eq!(guest_major_a, specdevices[0].major);
        assert_eq!(guest_minor_a, specdevices[0].minor);
        assert_eq!(guest_major_b, specdevices[1].major);
        assert_eq!(guest_minor_b, specdevices[1].minor);

        let specresources = spec.linux.as_ref().unwrap().resources.as_ref().unwrap();
        assert_eq!(Some(guest_major_a), specresources.devices[0].major);
        assert_eq!(Some(guest_minor_a), specresources.devices[0].minor);
        assert_eq!(Some(guest_major_b), specresources.devices[1].major);
        assert_eq!(Some(guest_minor_b), specresources.devices[1].minor);
    }

    #[test]
    fn test_update_spec_devices_char_block_conflict() {
        let null_rdev = fs::metadata("/dev/null").unwrap().rdev();

        let guest_major = stat::major(null_rdev) as i64;
        let guest_minor = stat::minor(null_rdev) as i64;
        let host_major: i64 = 99;
        let host_minor: i64 = 99;

        let mut spec = Spec {
            linux: Some(Linux {
                devices: vec![
                    oci::LinuxDevice {
                        path: "/dev/char".to_string(),
                        r#type: "c".to_string(),
                        major: host_major,
                        minor: host_minor,
                        ..oci::LinuxDevice::default()
                    },
                    oci::LinuxDevice {
                        path: "/dev/block".to_string(),
                        r#type: "b".to_string(),
                        major: host_major,
                        minor: host_minor,
                        ..oci::LinuxDevice::default()
                    },
                ],
                resources: Some(LinuxResources {
                    devices: vec![
                        LinuxDeviceCgroup {
                            r#type: "c".to_string(),
                            major: Some(host_major),
                            minor: Some(host_minor),
                            ..LinuxDeviceCgroup::default()
                        },
                        LinuxDeviceCgroup {
                            r#type: "b".to_string(),
                            major: Some(host_major),
                            minor: Some(host_minor),
                            ..LinuxDeviceCgroup::default()
                        },
                    ],
                    ..LinuxResources::default()
                }),
                ..Linux::default()
            }),
            ..Spec::default()
        };

        let container_path = "/dev/char";
        let vm_path = "/dev/null";

        let specresources = spec.linux.as_ref().unwrap().resources.as_ref().unwrap();
        assert_eq!(Some(host_major), specresources.devices[0].major);
        assert_eq!(Some(host_minor), specresources.devices[0].minor);
        assert_eq!(Some(host_major), specresources.devices[1].major);
        assert_eq!(Some(host_minor), specresources.devices[1].minor);

        let res = update_spec_devices(
            &mut spec,
            HashMap::from_iter(vec![(
                container_path,
                DevNumUpdate::from_vm_path(vm_path).unwrap().into(),
            )]),
        );
        assert!(res.is_ok());

        // Only the char device, not the block device should be updated
        let specresources = spec.linux.as_ref().unwrap().resources.as_ref().unwrap();
        assert_eq!(Some(guest_major), specresources.devices[0].major);
        assert_eq!(Some(guest_minor), specresources.devices[0].minor);
        assert_eq!(Some(host_major), specresources.devices[1].major);
        assert_eq!(Some(host_minor), specresources.devices[1].minor);
    }

    #[test]
    fn test_update_spec_devices_final_path() {
        let null_rdev = fs::metadata("/dev/null").unwrap().rdev();
        let guest_major = stat::major(null_rdev) as i64;
        let guest_minor = stat::minor(null_rdev) as i64;

        let container_path = "/dev/original";
        let host_major: i64 = 99;
        let host_minor: i64 = 99;

        let mut spec = Spec {
            linux: Some(Linux {
                devices: vec![oci::LinuxDevice {
                    path: container_path.to_string(),
                    r#type: "c".to_string(),
                    major: host_major,
                    minor: host_minor,
                    ..oci::LinuxDevice::default()
                }],
                ..Linux::default()
            }),
            ..Spec::default()
        };

        let vm_path = "/dev/null";
        let final_path = "/dev/new";

        let res = update_spec_devices(
            &mut spec,
            HashMap::from_iter(vec![(
                container_path,
                DevUpdate::from_vm_path(vm_path, final_path.to_string()).unwrap(),
            )]),
        );
        assert!(res.is_ok());

        let specdevices = &spec.linux.as_ref().unwrap().devices;
        assert_eq!(guest_major, specdevices[0].major);
        assert_eq!(guest_minor, specdevices[0].minor);
        assert_eq!(final_path, specdevices[0].path);
    }

    #[test]
    fn test_update_env_pci() {
        let example_map = [
            // Each is a host,guest pair of pci addresses
            ("0000:1a:01.0", "0000:01:01.0"),
            ("0000:1b:02.0", "0000:01:02.0"),
            // This one has the same host address as guest address
            // above, to test that we're not double-translating
            ("0000:01:01.0", "ffff:02:1f.7"),
        ];

        let mut env = vec![
            "PCIDEVICE_x=0000:1a:01.0,0000:1b:02.0".to_string(),
            "PCIDEVICE_y=0000:01:01.0".to_string(),
            "NOTAPCIDEVICE_blah=abcd:ef:01.0".to_string(),
        ];

        let pci_fixups = example_map
            .iter()
            .map(|(h, g)| {
                (
                    pci::Address::from_str(h).unwrap(),
                    pci::Address::from_str(g).unwrap(),
                )
            })
            .collect();

        let res = update_env_pci(&mut env, &pci_fixups);
        assert!(res.is_ok());

        assert_eq!(env[0], "PCIDEVICE_x=0000:01:01.0,0000:01:02.0");
        assert_eq!(env[1], "PCIDEVICE_y=ffff:02:1f.7");
        assert_eq!(env[2], "NOTAPCIDEVICE_blah=abcd:ef:01.0");
    }

    #[test]
    fn test_pcipath_to_sysfs() {
        let testdir = tempdir().expect("failed to create tmpdir");
        let rootbuspath = testdir.path().to_str().unwrap();

        let path2 = pci::Path::from_str("02").unwrap();
        let path23 = pci::Path::from_str("02/03").unwrap();
        let path234 = pci::Path::from_str("02/03/04").unwrap();

        let relpath = pcipath_to_sysfs(rootbuspath, &path2);
        assert_eq!(relpath.unwrap(), "/0000:00:02.0");

        let relpath = pcipath_to_sysfs(rootbuspath, &path23);
        assert!(relpath.is_err());

        let relpath = pcipath_to_sysfs(rootbuspath, &path234);
        assert!(relpath.is_err());

        // Create mock sysfs files for the device at 0000:00:02.0
        let bridge2path = format!("{}{}", rootbuspath, "/0000:00:02.0");

        fs::create_dir_all(&bridge2path).unwrap();

        let relpath = pcipath_to_sysfs(rootbuspath, &path2);
        assert_eq!(relpath.unwrap(), "/0000:00:02.0");

        let relpath = pcipath_to_sysfs(rootbuspath, &path23);
        assert!(relpath.is_err());

        let relpath = pcipath_to_sysfs(rootbuspath, &path234);
        assert!(relpath.is_err());

        // Create mock sysfs files to indicate that 0000:00:02.0 is a bridge to bus 01
        let bridge2bus = "0000:01";
        let bus2path = format!("{}/pci_bus/{}", bridge2path, bridge2bus);

        fs::create_dir_all(bus2path).unwrap();

        let relpath = pcipath_to_sysfs(rootbuspath, &path2);
        assert_eq!(relpath.unwrap(), "/0000:00:02.0");

        let relpath = pcipath_to_sysfs(rootbuspath, &path23);
        assert_eq!(relpath.unwrap(), "/0000:00:02.0/0000:01:03.0");

        let relpath = pcipath_to_sysfs(rootbuspath, &path234);
        assert!(relpath.is_err());

        // Create mock sysfs files for a bridge at 0000:01:03.0 to bus 02
        let bridge3path = format!("{}/0000:01:03.0", bridge2path);
        let bridge3bus = "0000:02";
        let bus3path = format!("{}/pci_bus/{}", bridge3path, bridge3bus);

        fs::create_dir_all(bus3path).unwrap();

        let relpath = pcipath_to_sysfs(rootbuspath, &path2);
        assert_eq!(relpath.unwrap(), "/0000:00:02.0");

        let relpath = pcipath_to_sysfs(rootbuspath, &path23);
        assert_eq!(relpath.unwrap(), "/0000:00:02.0/0000:01:03.0");

        let relpath = pcipath_to_sysfs(rootbuspath, &path234);
        assert_eq!(relpath.unwrap(), "/0000:00:02.0/0000:01:03.0/0000:02:04.0");
    }

    // We use device specific variants of this for real cases, but
    // they have some complications that make them troublesome to unit
    // test
    async fn example_get_device_name(
        sandbox: &Arc<Mutex<Sandbox>>,
        relpath: &str,
    ) -> Result<String> {
        let matcher = VirtioBlkPciMatcher::new(relpath);

        let uev = wait_for_uevent(sandbox, matcher).await?;

        Ok(uev.devname)
    }

    #[tokio::test]
    async fn test_get_device_name() {
        let devname = "vda";
        let root_bus = create_pci_root_bus_path();
        let relpath = "/0000:00:0a.0/0000:03:0b.0";
        let devpath = format!("{}{}/virtio4/block/{}", root_bus, relpath, devname);

        let mut uev = crate::uevent::Uevent::default();
        uev.action = crate::linux_abi::U_EVENT_ACTION_ADD.to_string();
        uev.subsystem = "block".to_string();
        uev.devpath = devpath.clone();
        uev.devname = devname.to_string();

        let logger = slog::Logger::root(slog::Discard, o!());
        let sandbox = Arc::new(Mutex::new(Sandbox::new(&logger).unwrap()));

        let mut sb = sandbox.lock().await;
        sb.uevent_map.insert(devpath.clone(), uev);
        drop(sb); // unlock

        let name = example_get_device_name(&sandbox, relpath).await;
        assert!(name.is_ok(), "{}", name.unwrap_err());
        assert_eq!(name.unwrap(), devname);

        let mut sb = sandbox.lock().await;
        let uev = sb.uevent_map.remove(&devpath).unwrap();
        drop(sb); // unlock

        spawn_test_watcher(sandbox.clone(), uev);

        let name = example_get_device_name(&sandbox, relpath).await;
        assert!(name.is_ok(), "{}", name.unwrap_err());
        assert_eq!(name.unwrap(), devname);
    }

    #[tokio::test]
    async fn test_virtio_blk_matcher() {
        let root_bus = create_pci_root_bus_path();
        let devname = "vda";

        let mut uev_a = crate::uevent::Uevent::default();
        let relpath_a = "/0000:00:0a.0";
        uev_a.action = crate::linux_abi::U_EVENT_ACTION_ADD.to_string();
        uev_a.subsystem = "block".to_string();
        uev_a.devname = devname.to_string();
        uev_a.devpath = format!("{}{}/virtio4/block/{}", root_bus, relpath_a, devname);
        let matcher_a = VirtioBlkPciMatcher::new(relpath_a);

        let mut uev_b = uev_a.clone();
        let relpath_b = "/0000:00:0a.0/0000:00:0b.0";
        uev_b.devpath = format!("{}{}/virtio0/block/{}", root_bus, relpath_b, devname);
        let matcher_b = VirtioBlkPciMatcher::new(relpath_b);

        assert!(matcher_a.is_match(&uev_a));
        assert!(matcher_b.is_match(&uev_b));
        assert!(!matcher_b.is_match(&uev_a));
        assert!(!matcher_a.is_match(&uev_b));
    }

    #[tokio::test]
    async fn test_scsi_block_matcher() {
        let root_bus = create_pci_root_bus_path();
        let devname = "sda";

        let mut uev_a = crate::uevent::Uevent::default();
        let addr_a = "0:0";
        uev_a.action = crate::linux_abi::U_EVENT_ACTION_ADD.to_string();
        uev_a.subsystem = "block".to_string();
        uev_a.devname = devname.to_string();
        uev_a.devpath = format!(
            "{}/0000:00:00.0/virtio0/host0/target0:0:0/0:0:{}/block/sda",
            root_bus, addr_a
        );
        let matcher_a = ScsiBlockMatcher::new(addr_a);

        let mut uev_b = uev_a.clone();
        let addr_b = "2:0";
        uev_b.devpath = format!(
            "{}/0000:00:00.0/virtio0/host0/target0:0:2/0:0:{}/block/sdb",
            root_bus, addr_b
        );
        let matcher_b = ScsiBlockMatcher::new(addr_b);

        assert!(matcher_a.is_match(&uev_a));
        assert!(matcher_b.is_match(&uev_b));
        assert!(!matcher_b.is_match(&uev_a));
        assert!(!matcher_a.is_match(&uev_b));
    }

    #[tokio::test]
    async fn test_vfio_matcher() {
        let grpa = IommuGroup(1);
        let grpb = IommuGroup(22);

        let mut uev_a = crate::uevent::Uevent::default();
        uev_a.action = crate::linux_abi::U_EVENT_ACTION_ADD.to_string();
        uev_a.devname = format!("vfio/{}", grpa);
        uev_a.devpath = format!("/devices/virtual/vfio/{}", grpa);
        let matcher_a = VfioMatcher::new(grpa);

        let mut uev_b = uev_a.clone();
        uev_b.devpath = format!("/devices/virtual/vfio/{}", grpb);
        let matcher_b = VfioMatcher::new(grpb);

        assert!(matcher_a.is_match(&uev_a));
        assert!(matcher_b.is_match(&uev_b));
        assert!(!matcher_b.is_match(&uev_a));
        assert!(!matcher_a.is_match(&uev_b));
    }

    #[test]
    fn test_split_vfio_option() {
        assert_eq!(
            split_vfio_option("0000:01:00.0=02/01"),
            Some(("0000:01:00.0", "02/01"))
        );
        assert_eq!(split_vfio_option("0000:01:00.0=02/01=rubbish"), None);
        assert_eq!(split_vfio_option("0000:01:00.0"), None);
    }

    #[test]
    fn test_pci_driver_override() {
        let testdir = tempdir().expect("failed to create tmpdir");
        let syspci = testdir.path(); // Path to mock /sys/bus/pci

        let dev0 = pci::Address::new(0, 0, pci::SlotFn::new(0, 0).unwrap());
        let dev0path = syspci.join("devices").join(dev0.to_string());
        let dev0drv = dev0path.join("driver");
        let dev0override = dev0path.join("driver_override");

        let drvapath = syspci.join("drivers").join("drv_a");
        let drvaunbind = drvapath.join("unbind");

        let probepath = syspci.join("drivers_probe");

        // Start mocking dev0 as being unbound
        fs::create_dir_all(&dev0path).unwrap();

        pci_driver_override(syspci, dev0, "drv_a").unwrap();
        assert_eq!(fs::read_to_string(&dev0override).unwrap(), "drv_a");
        assert_eq!(fs::read_to_string(&probepath).unwrap(), dev0.to_string());

        // Now mock dev0 already being attached to drv_a
        fs::create_dir_all(&drvapath).unwrap();
        std::os::unix::fs::symlink(&drvapath, dev0drv).unwrap();
        std::fs::remove_file(&probepath).unwrap();

        pci_driver_override(syspci, dev0, "drv_a").unwrap(); // no-op
        assert_eq!(fs::read_to_string(&dev0override).unwrap(), "drv_a");
        assert!(!probepath.exists());

        // Now try binding to a different driver
        pci_driver_override(syspci, dev0, "drv_b").unwrap();
        assert_eq!(fs::read_to_string(&dev0override).unwrap(), "drv_b");
        assert_eq!(fs::read_to_string(&probepath).unwrap(), dev0.to_string());
        assert_eq!(fs::read_to_string(&drvaunbind).unwrap(), dev0.to_string());
    }

    #[test]
    fn test_pci_iommu_group() {
        let testdir = tempdir().expect("failed to create tmpdir"); // mock /sys
        let syspci = testdir.path().join("bus").join("pci");

        // Mock dev0, which has no group
        let dev0 = pci::Address::new(0, 0, pci::SlotFn::new(0, 0).unwrap());
        let dev0path = syspci.join("devices").join(dev0.to_string());

        fs::create_dir_all(&dev0path).unwrap();

        // Test dev0
        assert!(pci_iommu_group(&syspci, dev0).unwrap().is_none());

        // Mock dev1, which is in group 12
        let dev1 = pci::Address::new(0, 1, pci::SlotFn::new(0, 0).unwrap());
        let dev1path = syspci.join("devices").join(dev1.to_string());
        let dev1group = dev1path.join("iommu_group");

        fs::create_dir_all(&dev1path).unwrap();
        std::os::unix::fs::symlink("../../../kernel/iommu_groups/12", &dev1group).unwrap();

        // Test dev1
        assert_eq!(
            pci_iommu_group(&syspci, dev1).unwrap(),
            Some(IommuGroup(12))
        );

        // Mock dev2, which has a bogus group (dir instead of symlink)
        let dev2 = pci::Address::new(0, 2, pci::SlotFn::new(0, 0).unwrap());
        let dev2path = syspci.join("devices").join(dev2.to_string());
        let dev2group = dev2path.join("iommu_group");

        fs::create_dir_all(&dev2group).unwrap();

        // Test dev2
        assert!(pci_iommu_group(&syspci, dev2).is_err());
    }
}
