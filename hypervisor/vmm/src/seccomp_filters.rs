// Copyright 2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Copyright © 2020 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0

use std::sync::RwLock;

use hypervisor::HypervisorType;
use lazy_static::lazy_static;
use seccompiler::{
    BackendError, BpfProgram, Error, SeccompAction, SeccompCmpArgLen as ArgLen, SeccompCmpOp::Eq,
    SeccompCondition as Cond, SeccompFilter, SeccompRule,
};
use std::convert::TryInto;

lazy_static! {
    static ref RUTIME_RULES: RwLock<Vec<(i64, Vec<SeccompRule>)>> = RwLock::new(Vec::new());
}

pub enum Thread {
    Api,
    SignalHandler,
    Vcpu,
    Vmm,
    PtyForeground,
    All,
}

/// Shorthand for chaining `SeccompCondition`s with the `and` operator  in a `SeccompRule`.
/// The rule will take the `Allow` action if _all_ the conditions are true.
///
/// [`SeccompCondition`]: struct.SeccompCondition.html
/// [`SeccompRule`]: struct.SeccompRule.html
macro_rules! and {
    ($($x:expr),*) => (SeccompRule::new(vec![$($x),*]).unwrap())
}

/// Shorthand for chaining `SeccompRule`s with the `or` operator in a `SeccompFilter`.
///
/// [`SeccompFilter`]: struct.SeccompFilter.html
/// [`SeccompRule`]: struct.SeccompRule.html
macro_rules! or {
    ($($x:expr,)*) => (vec![$($x),*]);
    ($($x:expr),*) => (vec![$($x),*])
}

// See include/uapi/asm-generic/ioctls.h in the kernel code.
const TCGETS: u64 = 0x5401;
const TCSETS: u64 = 0x5402;
const TIOCSCTTY: u64 = 0x540E;
const TIOCSPGRP: u64 = 0x5410;
const TIOCGWINSZ: u64 = 0x5413;
const TIOCSPTLCK: u64 = 0x4004_5431;
const TIOCGTPEER: u64 = 0x5441;
const FIOCLEX: u64 = 0x5451;
const FIONBIO: u64 = 0x5421;

// See include/uapi/linux/fs.h in the kernel code.
const BLKSSZGET: u64 = 0x1268;
const BLKPBSZGET: u64 = 0x127b;
const BLKIOMIN: u64 = 0x1278;
const BLKIOOPT: u64 = 0x1279;

// See include/uapi/linux/if_tun.h in the kernel code.
const TUNGETIFF: u64 = 0x8004_54d2;
const TUNSETIFF: u64 = 0x4004_54ca;
const TUNSETOFFLOAD: u64 = 0x4004_54d0;
const TUNSETVNETHDRSZ: u64 = 0x4004_54d8;
const TUNGETFEATURES: u64 = 0x8004_54cf;

// See include/uapi/linux/sockios.h in the kernel code.
const SIOCGIFFLAGS: u64 = 0x8913;
const SIOCGIFHWADDR: u64 = 0x8927;
const SIOCSIFFLAGS: u64 = 0x8914;
const SIOCSIFADDR: u64 = 0x8916;
const SIOCGIFMTU: u64 = 0x8921;
const SIOCSIFMTU: u64 = 0x8922;
const SIOCSIFHWADDR: u64 = 0x8924;
const SIOCSIFNETMASK: u64 = 0x891c;

// See include/uapi/linux/vfio.h in the kernel code.
const VFIO_GET_API_VERSION: u64 = 0x3b64;
const VFIO_CHECK_EXTENSION: u64 = 0x3b65;
const VFIO_SET_IOMMU: u64 = 0x3b66;
const VFIO_GROUP_GET_STATUS: u64 = 0x3b67;
const VFIO_GROUP_SET_CONTAINER: u64 = 0x3b68;
const VFIO_GROUP_UNSET_CONTAINER: u64 = 0x3b69;
const VFIO_GROUP_GET_DEVICE_FD: u64 = 0x3b6a;
const VFIO_DEVICE_GET_INFO: u64 = 0x3b6b;
const VFIO_DEVICE_GET_REGION_INFO: u64 = 0x3b6c;
const VFIO_DEVICE_GET_IRQ_INFO: u64 = 0x3b6d;
const VFIO_DEVICE_SET_IRQS: u64 = 0x3b6e;
const VFIO_DEVICE_RESET: u64 = 0x3b6f;
const VFIO_IOMMU_MAP_DMA: u64 = 0x3b71;
const VFIO_IOMMU_UNMAP_DMA: u64 = 0x3b72;
const VFIO_DEVICE_IOEVENTFD: u64 = 0x3b74;

// See include/uapi/linux/vhost.h in the kernel code
const VHOST_GET_FEATURES: u64 = 0x8008af00;
const VHOST_SET_FEATURES: u64 = 0x4008af00;
const VHOST_SET_OWNER: u64 = 0xaf01;
const VHOST_SET_VRING_NUM: u64 = 0x4008af10;
const VHOST_SET_VRING_ADDR: u64 = 0x4028af11;
const VHOST_SET_VRING_BASE: u64 = 0x4008af12;
const VHOST_SET_VRING_KICK: u64 = 0x4008af20;
const VHOST_SET_VRING_CALL: u64 = 0x4008af21;
const VHOST_SET_BACKEND_FEATURES: u64 = 0x4008af25;
const VHOST_GET_BACKEND_FEATURES: u64 = 0x8008af26;
const VHOST_VDPA_GET_DEVICE_ID: u64 = 0x8004af70;
const VHOST_VDPA_GET_STATUS: u64 = 0x8001af71;
const VHOST_VDPA_SET_STATUS: u64 = 0x4001af72;
const VHOST_VDPA_GET_CONFIG: u64 = 0x8008af73;
const VHOST_VDPA_SET_CONFIG: u64 = 0x4008af74;
const VHOST_VDPA_SET_VRING_ENABLE: u64 = 0x4008af75;
const VHOST_VDPA_GET_VRING_NUM: u64 = 0x8002af76;
const VHOST_VDPA_SET_CONFIG_CALL: u64 = 0x4004af77;
const VHOST_VDPA_GET_IOVA_RANGE: u64 = 0x8010af78;
const VHOST_VDPA_GET_CONFIG_SIZE: u64 = 0x8004af79;
const VHOST_VDPA_SUSPEND: u64 = 0xaf7d;

// See include/uapi/linux/kvm.h in the kernel code.
#[cfg(feature = "kvm")]
mod kvm {
    pub const KVM_GET_API_VERSION: u64 = 0xae00;
    pub const KVM_CREATE_VM: u64 = 0xae01;
    pub const KVM_CHECK_EXTENSION: u64 = 0xae03;
    pub const KVM_GET_VCPU_MMAP_SIZE: u64 = 0xae04;
    pub const KVM_CREATE_VCPU: u64 = 0xae41;
    pub const KVM_CREATE_IRQCHIP: u64 = 0xae60;
    pub const KVM_RUN: u64 = 0xae80;
    pub const KVM_SET_MP_STATE: u64 = 0x4004_ae99;
    pub const KVM_SET_GSI_ROUTING: u64 = 0x4008_ae6a;
    pub const KVM_SET_DEVICE_ATTR: u64 = 0x4018_aee1;
    pub const KVM_HAS_DEVICE_ATTR: u64 = 0x4018_aee3;
    pub const KVM_SET_ONE_REG: u64 = 0x4010_aeac;
    pub const KVM_SET_USER_MEMORY_REGION: u64 = 0x4020_ae46;
    pub const KVM_IRQFD: u64 = 0x4020_ae76;
    pub const KVM_IOEVENTFD: u64 = 0x4040_ae79;
    pub const KVM_SET_VCPU_EVENTS: u64 = 0x4040_aea0;
    pub const KVM_ENABLE_CAP: u64 = 0x4068_aea3;
    pub const KVM_SET_REGS: u64 = 0x4090_ae82;
    pub const KVM_GET_MP_STATE: u64 = 0x8004_ae98;
    pub const KVM_GET_DEVICE_ATTR: u64 = 0x4018_aee2;
    pub const KVM_GET_DIRTY_LOG: u64 = 0x4010_ae42;
    pub const KVM_GET_VCPU_EVENTS: u64 = 0x8040_ae9f;
    pub const KVM_GET_ONE_REG: u64 = 0x4010_aeab;
    pub const KVM_GET_REGS: u64 = 0x8090_ae81;
    pub const KVM_GET_SUPPORTED_CPUID: u64 = 0xc008_ae05;
    pub const KVM_CREATE_DEVICE: u64 = 0xc00c_aee0;
    pub const KVM_GET_REG_LIST: u64 = 0xc008_aeb0;
    pub const KVM_MEMORY_ENCRYPT_OP: u64 = 0xc008_aeba;
}

#[cfg(feature = "kvm")]
use kvm::*;

// MSHV IOCTL code. This is unstable until the kernel code has been declared stable.
#[cfg(feature = "mshv")]
mod mshv {
    pub const MSHV_GET_API_VERSION: u64 = 0xb800;
    pub const MSHV_CREATE_VM: u64 = 0x4028_b801;
    pub const MSHV_MAP_GUEST_MEMORY: u64 = 0x4020_b802;
    pub const MSHV_UNMAP_GUEST_MEMORY: u64 = 0x4020_b803;
    pub const MSHV_CREATE_VP: u64 = 0x4004_b804;
    pub const MSHV_IRQFD: u64 = 0x4010_b80e;
    pub const MSHV_IOEVENTFD: u64 = 0x4020_b80f;
    pub const MSHV_SET_MSI_ROUTING: u64 = 0x4008_b811;
    pub const MSHV_GET_VP_REGISTERS: u64 = 0xc010_b805;
    pub const MSHV_SET_VP_REGISTERS: u64 = 0x4010_b806;
    pub const MSHV_RUN_VP: u64 = 0x8100_b807;
    pub const MSHV_GET_VP_STATE: u64 = 0xc028_b80a;
    pub const MSHV_SET_VP_STATE: u64 = 0xc028_b80b;
    pub const MSHV_SET_PARTITION_PROPERTY: u64 = 0x4010_b80c;
    pub const MSHV_GET_GPA_ACCESS_STATES: u64 = 0xc01c_b812;
    pub const MSHV_VP_TRANSLATE_GVA: u64 = 0xc020_b80e;
    pub const MSHV_CREATE_PARTITION: u64 = 0x4030_b801;
}
#[cfg(feature = "mshv")]
use mshv::*;

#[cfg(feature = "mshv")]
fn create_vmm_ioctl_seccomp_rule_common_mshv() -> Result<Vec<SeccompRule>, BackendError> {
    Ok(or![
        and![Cond::new(1, ArgLen::Dword, Eq, MSHV_GET_API_VERSION,)?],
        and![Cond::new(1, ArgLen::Dword, Eq, MSHV_CREATE_VM)?],
        and![Cond::new(1, ArgLen::Dword, Eq, MSHV_MAP_GUEST_MEMORY)?],
        and![Cond::new(1, ArgLen::Dword, Eq, MSHV_UNMAP_GUEST_MEMORY)?],
        and![Cond::new(1, ArgLen::Dword, Eq, MSHV_CREATE_VP)?],
        and![Cond::new(1, ArgLen::Dword, Eq, MSHV_IRQFD)?],
        and![Cond::new(1, ArgLen::Dword, Eq, MSHV_IOEVENTFD)?],
        and![Cond::new(1, ArgLen::Dword, Eq, MSHV_SET_MSI_ROUTING)?],
        and![Cond::new(1, ArgLen::Dword, Eq, MSHV_GET_VP_REGISTERS)?],
        and![Cond::new(1, ArgLen::Dword, Eq, MSHV_SET_VP_REGISTERS)?],
        and![Cond::new(1, ArgLen::Dword, Eq, MSHV_RUN_VP)?],
        and![Cond::new(1, ArgLen::Dword, Eq, MSHV_GET_VP_STATE)?],
        and![Cond::new(1, ArgLen::Dword, Eq, MSHV_SET_VP_STATE)?],
        and![Cond::new(
            1,
            ArgLen::Dword,
            Eq,
            MSHV_SET_PARTITION_PROPERTY
        )?],
        and![Cond::new(1, ArgLen::Dword, Eq, MSHV_GET_GPA_ACCESS_STATES)?],
        and![Cond::new(1, ArgLen::Dword, Eq, MSHV_VP_TRANSLATE_GVA)?],
        and![Cond::new(1, ArgLen::Dword, Eq, MSHV_CREATE_PARTITION)?],
    ])
}

#[cfg(feature = "kvm")]
fn create_vmm_ioctl_seccomp_rule_common_kvm() -> Result<Vec<SeccompRule>, BackendError> {
    Ok(or![
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_CHECK_EXTENSION)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_CREATE_DEVICE,)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_CREATE_IRQCHIP,)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_CREATE_VCPU)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_CREATE_VM)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_ENABLE_CAP)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_GET_API_VERSION,)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_GET_DEVICE_ATTR,)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_GET_DIRTY_LOG)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_GET_MP_STATE)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_GET_ONE_REG)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_GET_REGS)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_GET_REG_LIST)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_GET_SUPPORTED_CPUID,)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_GET_VCPU_EVENTS,)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_GET_VCPU_MMAP_SIZE,)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_IOEVENTFD)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_IRQFD)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_RUN)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_MEMORY_ENCRYPT_OP)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_SET_DEVICE_ATTR,)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_HAS_DEVICE_ATTR,)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_SET_GSI_ROUTING)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_SET_MP_STATE)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_SET_ONE_REG)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_SET_REGS)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_SET_USER_MEMORY_REGION,)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_SET_VCPU_EVENTS,)?],
    ])
}

fn create_vmm_ioctl_seccomp_rule_hypervisor(
    hypervisor_type: HypervisorType,
) -> Result<Vec<SeccompRule>, BackendError> {
    match hypervisor_type {
        #[cfg(feature = "kvm")]
        HypervisorType::Kvm | HypervisorType::KvmPvm => create_vmm_ioctl_seccomp_rule_common_kvm(),
        #[cfg(feature = "mshv")]
        HypervisorType::Mshv => create_vmm_ioctl_seccomp_rule_common_mshv(),
        #[allow(unreachable_patterns)]
        _ => panic!("Invalid hypervisor {:?}", hypervisor_type),
    }
}

fn create_vmm_ioctl_seccomp_rule_common(
    hypervisor_type: HypervisorType,
) -> Result<Vec<SeccompRule>, BackendError> {
    let mut common_rules = or![
        and![Cond::new(1, ArgLen::Dword, Eq, BLKSSZGET)?],
        and![Cond::new(1, ArgLen::Dword, Eq, BLKPBSZGET)?],
        and![Cond::new(1, ArgLen::Dword, Eq, BLKIOMIN)?],
        and![Cond::new(1, ArgLen::Dword, Eq, BLKIOOPT)?],
        and![Cond::new(1, ArgLen::Dword, Eq, FIOCLEX)?],
        and![Cond::new(1, ArgLen::Dword, Eq, FIONBIO)?],
        and![Cond::new(1, ArgLen::Dword, Eq, SIOCGIFFLAGS)?],
        and![Cond::new(1, ArgLen::Dword, Eq, SIOCGIFHWADDR)?],
        and![Cond::new(1, ArgLen::Dword, Eq, SIOCGIFMTU)?],
        and![Cond::new(1, ArgLen::Dword, Eq, SIOCSIFADDR)?],
        and![Cond::new(1, ArgLen::Dword, Eq, SIOCSIFFLAGS)?],
        and![Cond::new(1, ArgLen::Dword, Eq, SIOCSIFHWADDR)?],
        and![Cond::new(1, ArgLen::Dword, Eq, SIOCSIFMTU)?],
        and![Cond::new(1, ArgLen::Dword, Eq, SIOCSIFNETMASK)?],
        and![Cond::new(1, ArgLen::Dword, Eq, TCSETS)?],
        and![Cond::new(1, ArgLen::Dword, Eq, TCGETS)?],
        and![Cond::new(1, ArgLen::Dword, Eq, TIOCGTPEER)?],
        and![Cond::new(1, ArgLen::Dword, Eq, TIOCGWINSZ)?],
        and![Cond::new(1, ArgLen::Dword, Eq, TIOCSCTTY)?],
        and![Cond::new(1, ArgLen::Dword, Eq, TIOCSPGRP)?],
        and![Cond::new(1, ArgLen::Dword, Eq, TIOCSPTLCK)?],
        and![Cond::new(1, ArgLen::Dword, Eq, TUNGETFEATURES)?],
        and![Cond::new(1, ArgLen::Dword, Eq, TUNGETIFF)?],
        and![Cond::new(1, ArgLen::Dword, Eq, TUNSETIFF)?],
        and![Cond::new(1, ArgLen::Dword, Eq, TUNSETOFFLOAD)?],
        and![Cond::new(1, ArgLen::Dword, Eq, TUNSETVNETHDRSZ)?],
        and![Cond::new(1, ArgLen::Dword, Eq, VFIO_GET_API_VERSION)?],
        and![Cond::new(1, ArgLen::Dword, Eq, VFIO_CHECK_EXTENSION)?],
        and![Cond::new(1, ArgLen::Dword, Eq, VFIO_SET_IOMMU)?],
        and![Cond::new(1, ArgLen::Dword, Eq, VFIO_GROUP_GET_STATUS)?],
        and![Cond::new(1, ArgLen::Dword, Eq, VFIO_GROUP_SET_CONTAINER)?],
        and![Cond::new(1, ArgLen::Dword, Eq, VFIO_GROUP_UNSET_CONTAINER)?],
        and![Cond::new(1, ArgLen::Dword, Eq, VFIO_GROUP_GET_DEVICE_FD)?],
        and![Cond::new(1, ArgLen::Dword, Eq, VFIO_DEVICE_GET_INFO)?],
        and![Cond::new(
            1,
            ArgLen::Dword,
            Eq,
            VFIO_DEVICE_GET_REGION_INFO
        )?],
        and![Cond::new(1, ArgLen::Dword, Eq, VFIO_DEVICE_GET_IRQ_INFO)?],
        and![Cond::new(1, ArgLen::Dword, Eq, VFIO_DEVICE_SET_IRQS)?],
        and![Cond::new(1, ArgLen::Dword, Eq, VFIO_DEVICE_RESET)?],
        and![Cond::new(1, ArgLen::Dword, Eq, VFIO_IOMMU_MAP_DMA)?],
        and![Cond::new(1, ArgLen::Dword, Eq, VFIO_IOMMU_UNMAP_DMA)?],
        and![Cond::new(1, ArgLen::Dword, Eq, VFIO_DEVICE_IOEVENTFD)?],
        and![Cond::new(1, ArgLen::Dword, Eq, VHOST_GET_FEATURES)?],
        and![Cond::new(1, ArgLen::Dword, Eq, VHOST_SET_FEATURES)?],
        and![Cond::new(1, ArgLen::Dword, Eq, VHOST_SET_OWNER)?],
        and![Cond::new(1, ArgLen::Dword, Eq, VHOST_SET_VRING_NUM)?],
        and![Cond::new(1, ArgLen::Dword, Eq, VHOST_SET_VRING_ADDR)?],
        and![Cond::new(1, ArgLen::Dword, Eq, VHOST_SET_VRING_BASE)?],
        and![Cond::new(1, ArgLen::Dword, Eq, VHOST_SET_VRING_KICK)?],
        and![Cond::new(1, ArgLen::Dword, Eq, VHOST_SET_VRING_CALL)?],
        and![Cond::new(1, ArgLen::Dword, Eq, VHOST_SET_BACKEND_FEATURES)?],
        and![Cond::new(1, ArgLen::Dword, Eq, VHOST_GET_BACKEND_FEATURES)?],
        and![Cond::new(1, ArgLen::Dword, Eq, VHOST_VDPA_GET_DEVICE_ID)?],
        and![Cond::new(1, ArgLen::Dword, Eq, VHOST_VDPA_GET_STATUS)?],
        and![Cond::new(1, ArgLen::Dword, Eq, VHOST_VDPA_SET_STATUS)?],
        and![Cond::new(1, ArgLen::Dword, Eq, VHOST_VDPA_GET_CONFIG)?],
        and![Cond::new(1, ArgLen::Dword, Eq, VHOST_VDPA_SET_CONFIG)?],
        and![Cond::new(
            1,
            ArgLen::Dword,
            Eq,
            VHOST_VDPA_SET_VRING_ENABLE
        )?],
        and![Cond::new(1, ArgLen::Dword, Eq, VHOST_VDPA_GET_VRING_NUM)?],
        and![Cond::new(1, ArgLen::Dword, Eq, VHOST_VDPA_SET_CONFIG_CALL)?],
        and![Cond::new(1, ArgLen::Dword, Eq, VHOST_VDPA_GET_IOVA_RANGE)?],
        and![Cond::new(1, ArgLen::Dword, Eq, VHOST_VDPA_GET_CONFIG_SIZE)?],
        and![Cond::new(1, ArgLen::Dword, Eq, VHOST_VDPA_SUSPEND)?],
    ];

    let hypervisor_rules = create_vmm_ioctl_seccomp_rule_hypervisor(hypervisor_type)?;

    common_rules.extend(hypervisor_rules);

    Ok(common_rules)
}

#[cfg(all(target_arch = "x86_64", feature = "kvm"))]
fn create_vmm_ioctl_seccomp_rule_kvm() -> Result<Vec<SeccompRule>, BackendError> {
    const KVM_CREATE_PIT2: u64 = 0x4040_ae77;
    const KVM_GET_CLOCK: u64 = 0x8030_ae7c;
    const KVM_GET_CPUID2: u64 = 0xc008_ae91;
    const KVM_GET_FPU: u64 = 0x81a0_ae8c;
    const KVM_GET_LAPIC: u64 = 0x8400_ae8e;
    const KVM_GET_MSR_INDEX_LIST: u64 = 0xc004_ae02;
    const KVM_GET_MSRS: u64 = 0xc008_ae88;
    const KVM_GET_SREGS: u64 = 0x8138_ae83;
    const KVM_GET_XCRS: u64 = 0x8188_aea6;
    const KVM_GET_XSAVE: u64 = 0x9000_aea4;
    const KVM_KVMCLOCK_CTRL: u64 = 0xaead;
    const KVM_SET_CLOCK: u64 = 0x4030_ae7b;
    const KVM_SET_CPUID2: u64 = 0x4008_ae90;
    const KVM_SET_FPU: u64 = 0x41a0_ae8d;
    const KVM_SET_IDENTITY_MAP_ADDR: u64 = 0x4008_ae48;
    const KVM_SET_LAPIC: u64 = 0x4400_ae8f;
    const KVM_SET_MSRS: u64 = 0x4008_ae89;
    const KVM_SET_SREGS: u64 = 0x4138_ae84;
    const KVM_SET_TSS_ADDR: u64 = 0xae47;
    const KVM_SET_XCRS: u64 = 0x4188_aea7;
    const KVM_SET_XSAVE: u64 = 0x5000_aea5;
    const KVM_SET_GUEST_DEBUG: u64 = 0x4048_ae9b;
    const KVM_TRANSLATE: u64 = 0xc018_ae85;

    let common_rules = create_vmm_ioctl_seccomp_rule_common(HypervisorType::Kvm)?;
    let mut arch_rules = or![
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_CREATE_PIT2)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_GET_CLOCK,)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_GET_CPUID2,)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_GET_FPU)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_GET_LAPIC)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_GET_MSR_INDEX_LIST)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_GET_MSRS)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_GET_SREGS)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_GET_XCRS,)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_GET_XSAVE,)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_KVMCLOCK_CTRL)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_SET_CLOCK)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_SET_CPUID2)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_SET_FPU)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_SET_IDENTITY_MAP_ADDR)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_SET_LAPIC)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_SET_SREGS)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_SET_TSS_ADDR,)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_SET_MSRS)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_SET_XCRS,)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_SET_XSAVE,)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_SET_GUEST_DEBUG,)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_TRANSLATE,)?],
    ];
    arch_rules.extend(common_rules);

    Ok(arch_rules)
}

#[cfg(all(target_arch = "aarch64", feature = "kvm"))]
fn create_vmm_ioctl_seccomp_rule_kvm() -> Result<Vec<SeccompRule>, BackendError> {
    const KVM_ARM_PREFERRED_TARGET: u64 = 0x8020_aeaf;
    const KVM_ARM_VCPU_INIT: u64 = 0x4020_aeae;
    const KVM_SET_GUEST_DEBUG: u64 = 0x4208_ae9b;

    let common_rules = create_vmm_ioctl_seccomp_rule_common(HypervisorType::Kvm)?;
    let mut arch_rules = or![
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_ARM_PREFERRED_TARGET,)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_ARM_VCPU_INIT,)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_SET_GUEST_DEBUG,)?],
    ];
    arch_rules.extend(common_rules);

    Ok(arch_rules)
}

#[cfg(all(target_arch = "x86_64", feature = "mshv"))]
fn create_vmm_ioctl_seccomp_rule_mshv() -> Result<Vec<SeccompRule>, BackendError> {
    create_vmm_ioctl_seccomp_rule_common(HypervisorType::Mshv)
}

fn create_vmm_ioctl_seccomp_rule(
    hypervisor_type: HypervisorType,
) -> Result<Vec<SeccompRule>, BackendError> {
    match hypervisor_type {
        #[cfg(feature = "kvm")]
        HypervisorType::Kvm | HypervisorType::KvmPvm => create_vmm_ioctl_seccomp_rule_kvm(),
        #[cfg(feature = "mshv")]
        HypervisorType::Mshv => create_vmm_ioctl_seccomp_rule_mshv(),
        #[allow(unreachable_patterns)]
        _ => panic!("Invalid hypervisor {:?}", hypervisor_type),
    }
}

fn create_api_ioctl_seccomp_rule() -> Result<Vec<SeccompRule>, BackendError> {
    Ok(or![and![Cond::new(1, ArgLen::Dword, Eq, FIONBIO)?]])
}

fn create_signal_handler_ioctl_seccomp_rule() -> Result<Vec<SeccompRule>, BackendError> {
    Ok(or![
        and![Cond::new(1, ArgLen::Dword, Eq, TCGETS)?],
        and![Cond::new(1, ArgLen::Dword, Eq, TCSETS)?],
        and![Cond::new(1, ArgLen::Dword, Eq, TIOCGWINSZ)?],
    ])
}

fn signal_handler_thread_rules() -> Result<Vec<(i64, Vec<SeccompRule>)>, BackendError> {
    Ok(vec![
        (libc::SYS_brk, vec![]),
        (libc::SYS_close, vec![]),
        (libc::SYS_exit, vec![]),
        (libc::SYS_exit_group, vec![]),
        (libc::SYS_futex, vec![]),
        (libc::SYS_ioctl, create_signal_handler_ioctl_seccomp_rule()?),
        (libc::SYS_madvise, vec![]),
        (libc::SYS_mmap, vec![]),
        (libc::SYS_munmap, vec![]),
        (libc::SYS_recvfrom, vec![]),
        (libc::SYS_rt_sigprocmask, vec![]),
        (libc::SYS_rt_sigreturn, vec![]),
        (libc::SYS_sendto, vec![]),
        (libc::SYS_sigaltstack, vec![]),
        (libc::SYS_write, vec![]),
    ])
}

fn create_pty_foreground_ioctl_seccomp_rule() -> Result<Vec<SeccompRule>, BackendError> {
    Ok(or![
        and![Cond::new(1, ArgLen::Dword, Eq, TIOCSCTTY)?],
        and![Cond::new(1, ArgLen::Dword, Eq, TIOCSPGRP)?],
    ])
}

fn pty_foreground_thread_rules() -> Result<Vec<(i64, Vec<SeccompRule>)>, BackendError> {
    Ok(vec![
        (libc::SYS_close, vec![]),
        (libc::SYS_exit_group, vec![]),
        (libc::SYS_getpgid, vec![]),
        #[cfg(target_arch = "x86_64")]
        (libc::SYS_getpgrp, vec![]),
        (libc::SYS_ioctl, create_pty_foreground_ioctl_seccomp_rule()?),
        (libc::SYS_munmap, vec![]),
        #[cfg(target_arch = "x86_64")]
        (libc::SYS_poll, vec![]),
        #[cfg(target_arch = "aarch64")]
        (libc::SYS_ppoll, vec![]),
        (libc::SYS_read, vec![]),
        (libc::SYS_rt_sigaction, vec![]),
        (libc::SYS_rt_sigreturn, vec![]),
        (libc::SYS_setsid, vec![]),
        (libc::SYS_sigaltstack, vec![]),
        (libc::SYS_write, vec![]),
    ])
}

// The filter containing the white listed syscall rules required by the VMM to
// function.
fn vmm_thread_rules(
    hypervisor_type: HypervisorType,
) -> Result<Vec<(i64, Vec<SeccompRule>)>, BackendError> {
    Ok(vec![
        (libc::SYS_accept4, vec![]),
        #[cfg(target_arch = "x86_64")]
        (libc::SYS_access, vec![]),
        (libc::SYS_bind, vec![]),
        (libc::SYS_brk, vec![]),
        (libc::SYS_clock_gettime, vec![]),
        (libc::SYS_clock_nanosleep, vec![]),
        (libc::SYS_clone, vec![]),
        (libc::SYS_clone3, vec![]),
        (libc::SYS_close, vec![]),
        (libc::SYS_connect, vec![]),
        (libc::SYS_dup, vec![]),
        (libc::SYS_epoll_create1, vec![]),
        (libc::SYS_epoll_ctl, vec![]),
        (libc::SYS_epoll_pwait, vec![]),
        #[cfg(target_arch = "x86_64")]
        (libc::SYS_epoll_wait, vec![]),
        (libc::SYS_eventfd2, vec![]),
        (libc::SYS_exit, vec![]),
        (libc::SYS_exit_group, vec![]),
        (libc::SYS_fallocate, vec![]),
        (libc::SYS_fcntl, vec![]),
        (libc::SYS_fdatasync, vec![]),
        (libc::SYS_fstat, vec![]),
        (libc::SYS_fsync, vec![]),
        (libc::SYS_ftruncate, vec![]),
        #[cfg(target_arch = "aarch64")]
        (libc::SYS_faccessat, vec![]),
        #[cfg(target_arch = "aarch64")]
        (libc::SYS_newfstatat, vec![]),
        (libc::SYS_futex, vec![]),
        (libc::SYS_getpgid, vec![]),
        #[cfg(target_arch = "x86_64")]
        (libc::SYS_getpgrp, vec![]),
        (libc::SYS_getpid, vec![]),
        (libc::SYS_getrandom, vec![]),
        (libc::SYS_gettid, vec![]),
        (libc::SYS_gettimeofday, vec![]),
        (libc::SYS_getuid, vec![]),
        (
            libc::SYS_ioctl,
            create_vmm_ioctl_seccomp_rule(hypervisor_type)?,
        ),
        (libc::SYS_getcwd, vec![]),
        (libc::SYS_io_uring_enter, vec![]),
        (libc::SYS_io_uring_setup, vec![]),
        (libc::SYS_io_uring_register, vec![]),
        (libc::SYS_kill, vec![]),
        (libc::SYS_listen, vec![]),
        (libc::SYS_lseek, vec![]),
        (libc::SYS_madvise, vec![]),
        (libc::SYS_mbind, vec![]),
        (libc::SYS_memfd_create, vec![]),
        (libc::SYS_mmap, vec![]),
        (libc::SYS_mprotect, vec![]),
        (libc::SYS_mremap, vec![]),
        (libc::SYS_munmap, vec![]),
        (libc::SYS_nanosleep, vec![]),
        (libc::SYS_newfstatat, vec![]),
        #[cfg(target_arch = "x86_64")]
        (libc::SYS_open, vec![]),
        (libc::SYS_openat, vec![]),
        (libc::SYS_pipe2, vec![]),
        #[cfg(target_arch = "x86_64")]
        (libc::SYS_poll, vec![]),
        #[cfg(target_arch = "aarch64")]
        (libc::SYS_ppoll, vec![]),
        (libc::SYS_prctl, vec![]),
        (libc::SYS_pread64, vec![]),
        (libc::SYS_preadv, vec![]),
        (libc::SYS_prlimit64, vec![]),
        (libc::SYS_pwrite64, vec![]),
        (libc::SYS_pwritev, vec![]),
        (libc::SYS_read, vec![]),
        (libc::SYS_readv, vec![]),
        #[cfg(target_arch = "x86_64")]
        (libc::SYS_readlink, vec![]),
        #[cfg(target_arch = "aarch64")]
        (libc::SYS_readlinkat, vec![]),
        (libc::SYS_recvfrom, vec![]),
        (libc::SYS_recvmsg, vec![]),
        (libc::SYS_restart_syscall, vec![]),
        // musl is missing this constant
        // (libc::SYS_rseq, vec![]),
        #[cfg(target_arch = "x86_64")]
        (334, vec![]),
        #[cfg(target_arch = "aarch64")]
        (293, vec![]),
        (libc::SYS_rt_sigaction, vec![]),
        (libc::SYS_rt_sigprocmask, vec![]),
        (libc::SYS_rt_sigreturn, vec![]),
        (libc::SYS_sched_getaffinity, vec![]),
        (libc::SYS_sched_setaffinity, vec![]),
        (libc::SYS_sendmsg, vec![]),
        (libc::SYS_sendto, vec![]),
        (libc::SYS_set_robust_list, vec![]),
        (libc::SYS_setsid, vec![]),
        (libc::SYS_shutdown, vec![]),
        (libc::SYS_sigaltstack, vec![]),
        (
            libc::SYS_socket,
            or![
                and![Cond::new(0, ArgLen::Dword, Eq, libc::AF_UNIX as u64)?],
                and![Cond::new(0, ArgLen::Dword, Eq, libc::AF_INET as u64)?],
            ],
        ),
        (libc::SYS_socketpair, vec![]),
        #[cfg(target_arch = "x86_64")]
        (libc::SYS_stat, vec![]),
        (libc::SYS_statx, vec![]),
        (libc::SYS_tgkill, vec![]),
        (libc::SYS_timerfd_create, vec![]),
        (libc::SYS_timerfd_settime, vec![]),
        (libc::SYS_tkill, vec![]),
        (
            libc::SYS_umask,
            or![and![Cond::new(0, ArgLen::Dword, Eq, 0o077)?]],
        ),
        #[cfg(any(
            target_arch = "x86_64",
            target_arch = "s390x",
            target_arch = "powerpc64le"
        ))]
        (libc::SYS_unlink, vec![]),
        (libc::SYS_unlinkat, vec![]),
        (libc::SYS_wait4, vec![]),
        (libc::SYS_write, vec![]),
        (libc::SYS_writev, vec![]),
        // native virtiofs stuff
        (libc::SYS_capget, vec![]), // For CAP_FSETID
        (libc::SYS_capset, vec![]),
        (libc::SYS_copy_file_range, vec![]),
        #[cfg(any(
            target_arch = "x86_64",
            target_arch = "s390x",
            target_arch = "powerpc64le"
        ))]
        (libc::SYS_epoll_create, vec![]),
        (libc::SYS_fchdir, vec![]),
        (libc::SYS_fchmod, vec![]),
        (libc::SYS_fchmodat, vec![]),
        (libc::SYS_fchownat, vec![]),
        (libc::SYS_fgetxattr, vec![]),
        (libc::SYS_flistxattr, vec![]),
        (libc::SYS_flock, vec![]),
        (libc::SYS_fremovexattr, vec![]),
        (libc::SYS_fsetxattr, vec![]),
        #[cfg(target_arch = "s390x")]
        (libc::SYS_fstatfs64, vec![]),
        (libc::SYS_fstatfs, vec![]),
        #[cfg(any(
            target_arch = "x86_64",
            target_arch = "s390x",
            target_arch = "powerpc64le"
        ))]
        (libc::SYS_getdents, vec![]),
        (libc::SYS_getdents64, vec![]),
        (libc::SYS_getegid, vec![]),
        (libc::SYS_geteuid, vec![]),
        (libc::SYS_getxattr, vec![]),
        (libc::SYS_linkat, vec![]),
        (libc::SYS_listxattr, vec![]),
        (libc::SYS_mkdirat, vec![]),
        (libc::SYS_mknodat, vec![]),
        (libc::SYS_name_to_handle_at, vec![]),
        (libc::SYS_openat2, vec![]),
        (libc::SYS_open_by_handle_at, vec![]),
        (libc::SYS_pwritev2, vec![]),
        (libc::SYS_renameat, vec![]),
        (libc::SYS_renameat2, vec![]),
        (libc::SYS_removexattr, vec![]),
        (libc::SYS_setresgid, vec![]),
        (libc::SYS_setresuid, vec![]),
        //(libc::SYS_setresgid32);  Needed on some platforms,
        //(libc::SYS_setresuid32);  Needed on some platforms
        (libc::SYS_setxattr, vec![]),
        #[cfg(target_arch = "s390x")]
        (libc::SYS_sigreturn, vec![]),
        (libc::SYS_symlinkat, vec![]),
        (libc::SYS_syncfs, vec![]),
        #[cfg(target_arch = "x86_64")]
        (libc::SYS_time, vec![]), // Rarely needed, except on static builds
        (libc::SYS_unshare, vec![]),
        (libc::SYS_utimensat, vec![]),
        // to support operations in init_backendfs()
        #[cfg(target_arch = "x86_64")]
        (libc::SYS_lstat, vec![]),
        // Async log.
        (libc::SYS_sched_yield, vec![]),
        (libc::SYS_setsockopt, vec![]),
    ])
}

#[cfg(feature = "kvm")]
fn create_vcpu_ioctl_seccomp_rule_kvm() -> Result<Vec<SeccompRule>, BackendError> {
    Ok(or![
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_CHECK_EXTENSION,)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_IOEVENTFD)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_IRQFD,)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_SET_DEVICE_ATTR,)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_SET_GSI_ROUTING,)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_SET_USER_MEMORY_REGION,)?],
        and![Cond::new(1, ArgLen::Dword, Eq, KVM_RUN,)?],
    ])
}

#[cfg(feature = "mshv")]
fn create_vcpu_ioctl_seccomp_rule_mshv() -> Result<Vec<SeccompRule>, BackendError> {
    Ok(or![
        and![Cond::new(1, ArgLen::Dword, Eq, MSHV_SET_MSI_ROUTING)?],
        and![Cond::new(1, ArgLen::Dword, Eq, MSHV_IOEVENTFD)?],
        and![Cond::new(1, ArgLen::Dword, Eq, MSHV_IRQFD)?],
        and![Cond::new(1, ArgLen::Dword, Eq, MSHV_RUN_VP)?],
        and![Cond::new(1, ArgLen::Dword, Eq, MSHV_GET_VP_REGISTERS)?],
        and![Cond::new(1, ArgLen::Dword, Eq, MSHV_SET_VP_REGISTERS)?],
        and![Cond::new(1, ArgLen::Dword, Eq, MSHV_MAP_GUEST_MEMORY)?],
        and![Cond::new(1, ArgLen::Dword, Eq, MSHV_UNMAP_GUEST_MEMORY)?],
        and![Cond::new(1, ArgLen::Dword, Eq, MSHV_VP_TRANSLATE_GVA)?],
    ])
}

fn create_vcpu_ioctl_seccomp_rule_hypervisor(
    hypervisor_type: HypervisorType,
) -> Result<Vec<SeccompRule>, BackendError> {
    match hypervisor_type {
        #[cfg(feature = "kvm")]
        HypervisorType::Kvm | HypervisorType::KvmPvm => create_vcpu_ioctl_seccomp_rule_kvm(),
        #[cfg(feature = "mshv")]
        HypervisorType::Mshv => create_vcpu_ioctl_seccomp_rule_mshv(),
        #[allow(unreachable_patterns)]
        _ => panic!("Invalid hypervisor {:?}", hypervisor_type),
    }
}

fn create_vcpu_ioctl_seccomp_rule(
    hypervisor_type: HypervisorType,
) -> Result<Vec<SeccompRule>, BackendError> {
    let mut rules = or![
        and![Cond::new(1, ArgLen::Dword, Eq, VFIO_DEVICE_SET_IRQS)?],
        and![Cond::new(1, ArgLen::Dword, Eq, VFIO_GROUP_UNSET_CONTAINER)?],
        and![Cond::new(1, ArgLen::Dword, Eq, VFIO_IOMMU_UNMAP_DMA)?],
        and![Cond::new(1, ArgLen::Dword, Eq, VHOST_VDPA_SET_STATUS)?],
        and![Cond::new(1, ArgLen::Dword, Eq, VHOST_VDPA_GET_CONFIG)?],
        and![Cond::new(1, ArgLen::Dword, Eq, VHOST_VDPA_SET_CONFIG)?],
        and![Cond::new(
            1,
            ArgLen::Dword,
            Eq,
            VHOST_VDPA_SET_VRING_ENABLE
        )?],
    ];

    let hypervisor_rules = create_vcpu_ioctl_seccomp_rule_hypervisor(hypervisor_type)?;

    rules.extend(hypervisor_rules);

    Ok(rules)
}

fn vcpu_thread_rules(
    hypervisor_type: HypervisorType,
) -> Result<Vec<(i64, Vec<SeccompRule>)>, BackendError> {
    Ok(vec![
        (libc::SYS_brk, vec![]),
        (libc::SYS_clock_gettime, vec![]),
        (libc::SYS_clock_nanosleep, vec![]),
        (libc::SYS_close, vec![]),
        (libc::SYS_dup, vec![]),
        (libc::SYS_exit, vec![]),
        (libc::SYS_epoll_ctl, vec![]),
        (libc::SYS_fstat, vec![]),
        (libc::SYS_futex, vec![]),
        (libc::SYS_getrandom, vec![]),
        (libc::SYS_getpid, vec![]),
        (
            libc::SYS_ioctl,
            create_vcpu_ioctl_seccomp_rule(hypervisor_type)?,
        ),
        (libc::SYS_lseek, vec![]),
        (libc::SYS_madvise, vec![]),
        (libc::SYS_mmap, vec![]),
        (libc::SYS_mprotect, vec![]),
        (libc::SYS_munmap, vec![]),
        (libc::SYS_nanosleep, vec![]),
        (libc::SYS_newfstatat, vec![]),
        #[cfg(target_arch = "x86_64")]
        (libc::SYS_open, vec![]),
        (libc::SYS_openat, vec![]),
        (libc::SYS_pread64, vec![]),
        (libc::SYS_pwrite64, vec![]),
        (libc::SYS_read, vec![]),
        (libc::SYS_recvfrom, vec![]),
        (libc::SYS_recvmsg, vec![]),
        (libc::SYS_rt_sigaction, vec![]),
        (libc::SYS_rt_sigprocmask, vec![]),
        (libc::SYS_rt_sigreturn, vec![]),
        (libc::SYS_sendmsg, vec![]),
        (libc::SYS_shutdown, vec![]),
        (libc::SYS_sigaltstack, vec![]),
        (libc::SYS_tgkill, vec![]),
        (libc::SYS_tkill, vec![]),
        #[cfg(target_arch = "x86_64")]
        (libc::SYS_unlink, vec![]),
        #[cfg(target_arch = "aarch64")]
        (libc::SYS_unlinkat, vec![]),
        (libc::SYS_write, vec![]),
        (libc::SYS_writev, vec![]),
    ])
}

// The filter containing the white listed syscall rules required by the API to
// function.
fn api_thread_rules() -> Result<Vec<(i64, Vec<SeccompRule>)>, BackendError> {
    Ok(vec![
        (libc::SYS_accept4, vec![]),
        (libc::SYS_brk, vec![]),
        (libc::SYS_close, vec![]),
        (libc::SYS_dup, vec![]),
        (libc::SYS_epoll_create1, vec![]),
        (libc::SYS_epoll_ctl, vec![]),
        (libc::SYS_epoll_pwait, vec![]),
        #[cfg(target_arch = "x86_64")]
        (libc::SYS_epoll_wait, vec![]),
        (libc::SYS_exit, vec![]),
        (libc::SYS_fcntl, vec![]),
        (libc::SYS_futex, vec![]),
        (libc::SYS_getrandom, vec![]),
        (libc::SYS_ioctl, create_api_ioctl_seccomp_rule()?),
        (libc::SYS_madvise, vec![]),
        (libc::SYS_mmap, vec![]),
        (libc::SYS_mprotect, vec![]),
        (libc::SYS_munmap, vec![]),
        (libc::SYS_recvfrom, vec![]),
        (libc::SYS_recvmsg, vec![]),
        (libc::SYS_sigaltstack, vec![]),
        (libc::SYS_write, vec![]),
    ])
}

fn thread_rules(
    hypervisor_type: HypervisorType,
) -> Result<Vec<(i64, Vec<SeccompRule>)>, BackendError> {
    let mut rules = api_thread_rules()?;
    rules.append(&mut signal_handler_thread_rules()?);
    rules.retain(|(num, _)| *num != libc::SYS_ioctl);
    rules.append(&mut vcpu_thread_rules(hypervisor_type).unwrap());
    rules.retain(|(num, _)| *num != libc::SYS_ioctl);
    rules.append(&mut vmm_thread_rules(hypervisor_type).unwrap());
    rules.retain(|(num, _)| *num != libc::SYS_ioctl);
    rules.append(&mut pty_foreground_thread_rules().unwrap());
    rules.retain(|(num, _)| *num != libc::SYS_ioctl);
    rules.append(&mut virtio_devices::seccomp_filters::virtio_device_thread_rules());
    rules.append(&mut create_runtime_seccomp_rules()?);
    rules.retain(|(num, _)| *num != libc::SYS_ioctl);

    let mut ioctl_rule = vec![];
    let rule = create_signal_handler_ioctl_seccomp_rule()?;
    ioctl_rule.extend(rule);
    let rule = create_pty_foreground_ioctl_seccomp_rule()?;
    ioctl_rule.extend(rule);
    let rule = create_vmm_ioctl_seccomp_rule(hypervisor_type)?;
    ioctl_rule.extend(rule);
    let rule = create_vcpu_ioctl_seccomp_rule(hypervisor_type)?;
    ioctl_rule.extend(rule);
    let rule = create_api_ioctl_seccomp_rule()?;
    ioctl_rule.extend(rule);
    let rule = virtio_devices::seccomp_filters::create_virtio_device_ioctl_seccomp_rule();
    ioctl_rule.extend(rule);

    let mut ioctl_rules = vec![(libc::SYS_ioctl, ioctl_rule)];
    rules.append(&mut ioctl_rules);

    Ok(rules)
}

fn create_runtime_seccomp_rules() -> Result<Vec<(i64, Vec<SeccompRule>)>, BackendError> {
    Ok(RUTIME_RULES.read().unwrap().to_vec())
}

pub fn set_runtime_seccomp_rules(rules: Vec<(i64, Vec<SeccompRule>)>) {
    *RUTIME_RULES.write().unwrap() = rules;
}

fn get_seccomp_rules(
    thread_type: Thread,
    hypervisor_type: HypervisorType,
) -> Result<Vec<(i64, Vec<SeccompRule>)>, BackendError> {
    match thread_type {
        Thread::Api => Ok(api_thread_rules()?),
        Thread::SignalHandler => Ok(signal_handler_thread_rules()?),
        Thread::Vcpu => Ok(vcpu_thread_rules(hypervisor_type)?),
        Thread::Vmm => Ok(vmm_thread_rules(hypervisor_type)?),
        Thread::PtyForeground => Ok(pty_foreground_thread_rules()?),
        Thread::All => Ok(thread_rules(hypervisor_type)?),
    }
}

/// Generate a BPF program based on the seccomp_action value
pub fn get_seccomp_filter(
    seccomp_action: &SeccompAction,
    thread_type: Thread,
    hypervisor_type: HypervisorType,
) -> Result<BpfProgram, Error> {
    match seccomp_action {
        SeccompAction::Allow => Ok(vec![]),
        SeccompAction::Log => SeccompFilter::new(
            get_seccomp_rules(thread_type, hypervisor_type)
                .map_err(Error::Backend)?
                .into_iter()
                .collect(),
            SeccompAction::Log,
            SeccompAction::Allow,
            std::env::consts::ARCH.try_into().unwrap(),
        )
        .and_then(|filter| filter.try_into())
        .map_err(Error::Backend),
        SeccompAction::KillProcess => match thread_type {
            Thread::All => SeccompFilter::new(
                get_seccomp_rules(thread_type, hypervisor_type)
                    .map_err(Error::Backend)?
                    .into_iter()
                    .collect(),
                SeccompAction::Trap,
                SeccompAction::Allow,
                std::env::consts::ARCH.try_into().unwrap(),
            )
            .and_then(|filter| filter.try_into())
            .map_err(Error::Backend),
            _ => Ok(vec![]),
        },
        _ => match thread_type {
            Thread::All => Ok(vec![]),
            _ => SeccompFilter::new(
                get_seccomp_rules(thread_type, hypervisor_type)
                    .map_err(Error::Backend)?
                    .into_iter()
                    .collect(),
                SeccompAction::Trap,
                SeccompAction::Allow,
                std::env::consts::ARCH.try_into().unwrap(),
            )
            .and_then(|filter| filter.try_into())
            .map_err(Error::Backend),
        },
    }
}
