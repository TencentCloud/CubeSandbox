// Copyright 2022 Akira Moroo.
// Portions Copyright 2020 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE-BSD-3-Clause file.
//
// SPDX-License-Identifier: BSD-3-Clause

use gdbstub::{
    arch::Arch,
    common::{Signal, Tid},
    conn::{Connection, ConnectionExt},
    stub::{run_blocking, DisconnectReason, MultiThreadStopReason},
    target::{
        ext::{
            base::{
                multithread::{
                    MultiThreadBase, MultiThreadResume, MultiThreadResumeOps,
                    MultiThreadSingleStep, MultiThreadSingleStepOps,
                },
                BaseOps,
            },
            breakpoints::{Breakpoints, BreakpointsOps, HwBreakpoint, HwBreakpointOps},
        },
        Target, TargetError, TargetResult,
    },
};
#[cfg(target_arch = "aarch64")]
use gdbstub_arch::aarch64::reg::AArch64CoreRegs as CoreRegs;
#[cfg(target_arch = "aarch64")]
use gdbstub_arch::aarch64::AArch64 as GdbArch;
#[cfg(target_arch = "x86_64")]
use gdbstub_arch::x86::reg::X86_64CoreRegs as CoreRegs;
#[cfg(target_arch = "x86_64")]
use gdbstub_arch::x86::X86_64_SSE as GdbArch;
use std::os::unix::net::UnixListener;
use std::sync::mpsc::{channel, RecvError, RecvTimeoutError, Sender};
use vm_memory::{GuestAddress, GuestMemoryError};

type ArchUsize = u64;

#[derive(Debug)]
pub enum DebuggableError {
    SetDebug(hypervisor::HypervisorCpuError),
    Pause(vm_migration::MigratableError),
    Resume(vm_migration::MigratableError),
    ReadRegs(crate::cpu::Error),
    WriteRegs(crate::cpu::Error),
    ReadMem(GuestMemoryError),
    WriteMem(GuestMemoryError),
    TranslateGva(crate::cpu::Error),
    PoisonedState,
}

pub trait Debuggable: vm_migration::Pausable {
    fn set_guest_debug(
        &self,
        cpu_id: usize,
        addrs: &[GuestAddress],
        singlestep: bool,
    ) -> Result<(), DebuggableError>;
    fn debug_pause(&mut self) -> std::result::Result<(), DebuggableError>;
    fn debug_resume(&mut self) -> std::result::Result<(), DebuggableError>;
    fn read_regs(&self, cpu_id: usize) -> std::result::Result<CoreRegs, DebuggableError>;
    fn write_regs(
        &self,
        cpu_id: usize,
        regs: &CoreRegs,
    ) -> std::result::Result<(), DebuggableError>;
    fn read_mem(
        &self,
        cpu_id: usize,
        vaddr: GuestAddress,
        len: usize,
    ) -> std::result::Result<Vec<u8>, DebuggableError>;
    fn write_mem(
        &self,
        cpu_id: usize,
        vaddr: &GuestAddress,
        data: &[u8],
    ) -> std::result::Result<(), DebuggableError>;
    fn active_vcpus(&self) -> usize;
}

#[derive(Debug)]
pub enum Error {
    Vm(crate::vm::Error),
    GdbRequest,
    GdbResponseNotify(std::io::Error),
    GdbResponse(RecvError),
    GdbResponseTimeout(RecvTimeoutError),
}
type GdbResult<T> = std::result::Result<T, Error>;

#[derive(Debug)]
pub struct GdbRequest {
    pub sender: Sender<GdbResponse>,
    pub payload: GdbRequestPayload,
    pub cpu_id: usize,
}

#[derive(Debug)]
pub enum GdbRequestPayload {
    ReadRegs,
    WriteRegs(Box<CoreRegs>),
    ReadMem(GuestAddress, usize),
    WriteMem(GuestAddress, Vec<u8>),
    Pause,
    Resume,
    SetSingleStep(bool),
    SetHwBreakPoint(Vec<GuestAddress>),
    ActiveVcpus,
}

pub type GdbResponse = std::result::Result<GdbResponsePayload, Error>;

#[derive(Debug)]
pub enum GdbResponsePayload {
    CommandComplete,
    RegValues(Box<CoreRegs>),
    MemoryRegion(Vec<u8>),
    ActiveVcpus(usize),
}

pub struct GdbStub {
    gdb_sender: Sender<GdbRequest>,
    gdb_event: vmm_sys_util::eventfd::EventFd,
    vm_event: vmm_sys_util::eventfd::EventFd,
    hw_breakpoints: Vec<GuestAddress>,
    single_step: bool,
}

impl GdbStub {
    pub fn new(
        gdb_sender: Sender<GdbRequest>,
        gdb_event: vmm_sys_util::eventfd::EventFd,
        vm_event: vmm_sys_util::eventfd::EventFd,
        hw_breakpoints: usize,
    ) -> Self {
        Self {
            gdb_sender,
            gdb_event,
            vm_event,
            hw_breakpoints: Vec::with_capacity(hw_breakpoints),
            single_step: false,
        }
    }

    fn vm_request(
        &self,
        payload: GdbRequestPayload,
        cpu_id: usize,
    ) -> GdbResult<GdbResponsePayload> {
        let (response_sender, response_receiver) = channel();
        let request = GdbRequest {
            sender: response_sender,
            payload,
            cpu_id,
        };
        self.gdb_sender
            .send(request)
            .map_err(|_| Error::GdbRequest)?;
        self.gdb_event.write(1).map_err(Error::GdbResponseNotify)?;
        let res = response_receiver.recv().map_err(Error::GdbResponse)??;
        Ok(res)
    }
}

impl Target for GdbStub {
    type Arch = GdbArch;
    type Error = String;

    #[inline(always)]
    fn base_ops(&mut self) -> BaseOps<Self::Arch, Self::Error> {
        BaseOps::MultiThread(self)
    }

    #[inline(always)]
    fn support_breakpoints(&mut self) -> Option<BreakpointsOps<Self>> {
        Some(self)
    }

    #[inline(always)]
    fn guard_rail_implicit_sw_breakpoints(&self) -> bool {
        true
    }
}

fn tid_to_cpuid(tid: Tid) -> usize {
    tid.get() - 1
}

fn cpuid_to_tid(cpu_id: usize) -> Tid {
    Tid::new(get_raw_tid(cpu_id)).unwrap()
}

pub fn get_raw_tid(cpu_id: usize) -> usize {
    cpu_id + 1
}

impl MultiThreadBase for GdbStub {
    fn read_registers(
        &mut self,
        regs: &mut <Self::Arch as Arch>::Registers,
        tid: Tid,
    ) -> TargetResult<(), Self> {
        match self.vm_request(GdbRequestPayload::ReadRegs, tid_to_cpuid(tid)) {
            Ok(GdbResponsePayload::RegValues(r)) => {
                *regs = *r;
                Ok(())
            }
            Ok(s) => {
                error!("Unexpected response for ReadRegs: {:?}", s);
                Err(TargetError::NonFatal)
            }
            Err(e) => {
                error!("Failed to request ReadRegs: {:?}", e);
                Err(TargetError::NonFatal)
            }
        }
    }

    fn write_registers(
        &mut self,
        regs: &<Self::Arch as Arch>::Registers,
        tid: Tid,
    ) -> TargetResult<(), Self> {
        match self.vm_request(
            GdbRequestPayload::WriteRegs(Box::new(regs.clone())),
            tid_to_cpuid(tid),
        ) {
            Ok(_) => Ok(()),
            Err(e) => {
                error!("Failed to request WriteRegs: {:?}", e);
                Err(TargetError::NonFatal)
            }
        }
    }

    fn read_addrs(
        &mut self,
        start_addr: <Self::Arch as Arch>::Usize,
        data: &mut [u8],
        tid: Tid,
    ) -> TargetResult<(), Self> {
        match self.vm_request(
            GdbRequestPayload::ReadMem(GuestAddress(start_addr), data.len()),
            tid_to_cpuid(tid),
        ) {
            Ok(GdbResponsePayload::MemoryRegion(r)) => {
                for (dst, v) in data.iter_mut().zip(r.iter()) {
                    *dst = *v;
                }
                Ok(())
            }
            Ok(s) => {
                error!("Unexpected response for ReadMem: {:?}", s);
                Err(TargetError::NonFatal)
            }
            Err(e) => {
                error!("Failed to request ReadMem: {:?}", e);
                Err(TargetError::NonFatal)
            }
        }
    }

    fn write_addrs(
        &mut self,
        start_addr: <Self::Arch as Arch>::Usize,
        data: &[u8],
        tid: Tid,
    ) -> TargetResult<(), Self> {
        match self.vm_request(
            GdbRequestPayload::WriteMem(GuestAddress(start_addr), data.to_owned()),
            tid_to_cpuid(tid),
        ) {
            Ok(_) => Ok(()),
            Err(e) => {
                error!("Failed to request WriteMem: {:?}", e);
                Err(TargetError::NonFatal)
            }
        }
    }

    fn list_active_threads(
        &mut self,
        thread_is_active: &mut dyn FnMut(Tid),
    ) -> Result<(), Self::Error> {
        match self.vm_request(GdbRequestPayload::ActiveVcpus, 0) {
            Ok(GdbResponsePayload::ActiveVcpus(active_vcpus)) => {
                (0..active_vcpus).for_each(|cpu_id| {
                    thread_is_active(cpuid_to_tid(cpu_id));
                });
                Ok(())
            }
            Ok(s) => Err(format!("Unexpected response for ActiveVcpus: {:?}", s)),
            Err(e) => Err(format!("Failed to request ActiveVcpus: {:?}", e)),
        }
    }

    #[inline(always)]
    fn support_resume(&mut self) -> Option<MultiThreadResumeOps<'_, Self>> {
        Some(self)
    }
}

impl MultiThreadResume for GdbStub {
    fn resume(&mut self) -> Result<(), Self::Error> {
        match self.vm_request(GdbRequestPayload::Resume, 0) {
            Ok(_) => Ok(()),
            Err(e) => Err(format!("Failed to resume the target: {:?}", e)),
        }
    }

    fn clear_resume_actions(&mut self) -> Result<(), Self::Error> {
        if self.single_step {
            match self.vm_request(GdbRequestPayload::SetSingleStep(false), 0) {
                Ok(_) => {
                    self.single_step = false;
                }
                Err(e) => {
                    return Err(format!("Failed to request SetSingleStep: {:?}", e));
                }
            }
        }
        Ok(())
    }

    fn set_resume_action_continue(
        &mut self,
        tid: Tid,
        signal: Option<Signal>,
    ) -> Result<(), Self::Error> {
        if signal.is_some() {
            return Err("no support for continuing with signal".to_owned());
        }
        match self.vm_request(GdbRequestPayload::Resume, tid_to_cpuid(tid)) {
            Ok(_) => Ok(()),
            Err(e) => Err(format!("Failed to resume the target: {:?}", e)),
        }
    }

    #[inline(always)]
    fn support_single_step(&mut self) -> Option<MultiThreadSingleStepOps<'_, Self>> {
        Some(self)
    }
}

impl MultiThreadSingleStep for GdbStub {
    fn set_resume_action_step(
        &mut self,
        tid: Tid,
        signal: Option<Signal>,
    ) -> Result<(), Self::Error> {
        if signal.is_some() {
            return Err("no support for stepping with signal".to_owned());
        }

        if !self.single_step {
            match self.vm_request(GdbRequestPayload::SetSingleStep(true), tid_to_cpuid(tid)) {
                Ok(_) => {
                    self.single_step = true;
                }
                Err(e) => {
                    return Err(format!("Failed to request SetSingleStep: {:?}", e));
                }
            }
        }
        match self.vm_request(GdbRequestPayload::Resume, tid_to_cpuid(tid)) {
            Ok(_) => Ok(()),
            Err(e) => Err(format!("Failed to resume the target: {:?}", e)),
        }
    }
}

impl Breakpoints for GdbStub {
    #[inline(always)]
    fn support_hw_breakpoint(&mut self) -> Option<HwBreakpointOps<Self>> {
        Some(self)
    }
}

impl HwBreakpoint for GdbStub {
    fn add_hw_breakpoint(
        &mut self,
        addr: <Self::Arch as Arch>::Usize,
        _kind: <Self::Arch as Arch>::BreakpointKind,
    ) -> TargetResult<bool, Self> {
        // If the HW breakpoints reach the limit, no more can be added.
        if self.hw_breakpoints.len() >= self.hw_breakpoints.capacity() {
            error!(
                "Not allowed to set more than {} HW breakpoints",
                self.hw_breakpoints.capacity()
            );
            return Ok(false);
        }

        self.hw_breakpoints.push(GuestAddress(addr));

        let payload = GdbRequestPayload::SetHwBreakPoint(self.hw_breakpoints.clone());
        match self.vm_request(payload, 0) {
            Ok(_) => Ok(true),
            Err(e) => {
                error!("Failed to request SetHwBreakPoint: {:?}", e);
                Err(TargetError::NonFatal)
            }
        }
    }
    fn remove_hw_breakpoint(
        &mut self,
        addr: <Self::Arch as Arch>::Usize,
        _kind: <Self::Arch as Arch>::BreakpointKind,
    ) -> TargetResult<bool, Self> {
        match self.hw_breakpoints.iter().position(|&b| b.0 == addr) {
            None => return Ok(false),
            Some(pos) => self.hw_breakpoints.remove(pos),
        };

        let payload = GdbRequestPayload::SetHwBreakPoint(self.hw_breakpoints.clone());
        match self.vm_request(payload, 0) {
            Ok(_) => Ok(true),
            Err(e) => {
                error!("Failed to request SetHwBreakPoint: {:?}", e);
                Err(TargetError::NonFatal)
            }
        }
    }
}

enum GdbEventLoop {}

impl run_blocking::BlockingEventLoop for GdbEventLoop {
    type Target = GdbStub;
    type Connection = Box<dyn ConnectionExt<Error = std::io::Error>>;
    type StopReason = MultiThreadStopReason<ArchUsize>;

    #[allow(clippy::type_complexity)]
    fn wait_for_stop_reason(
        target: &mut Self::Target,
        conn: &mut Self::Connection,
    ) -> Result<
        run_blocking::Event<Self::StopReason>,
        run_blocking::WaitForStopReasonError<
            <Self::Target as Target>::Error,
            <Self::Connection as Connection>::Error,
        >,
    > {
        // Polling
        loop {
            // This read is non-blocking.
            match target.vm_event.read() {
                Ok(tid) => {
                    target
                        .vm_request(GdbRequestPayload::Pause, 0)
                        .map_err(|_| {
                            run_blocking::WaitForStopReasonError::Target(
                                "Failed to pause VM".to_owned(),
                            )
                        })?;
                    let stop_reason = if target.single_step {
                        MultiThreadStopReason::DoneStep
                    } else {
                        MultiThreadStopReason::HwBreak(Tid::new(tid as usize).unwrap())
                    };
                    return Ok(run_blocking::Event::TargetStopped(stop_reason));
                }
                Err(e) => {
                    if e.kind() != std::io::ErrorKind::WouldBlock {
                        return Err(run_blocking::WaitForStopReasonError::Connection(e));
                    }
                }
            }

            if conn.peek().map(|b| b.is_some()).unwrap_or(true) {
                let byte = conn
                    .read()
                    .map_err(run_blocking::WaitForStopReasonError::Connection)?;
                return Ok(run_blocking::Event::IncomingData(byte));
            }
        }
    }

    fn on_interrupt(
        target: &mut Self::Target,
    ) -> Result<Option<Self::StopReason>, <Self::Target as Target>::Error> {
        target
            .vm_request(GdbRequestPayload::Pause, 0)
            .map_err(|e| {
                error!("Failed to pause the target: {:?}", e);
                "Failed to pause the target"
            })?;
        Ok(Some(MultiThreadStopReason::Signal(Signal::SIGINT)))
    }
}

pub fn gdb_thread(mut gdbstub: GdbStub, path: &std::path::Path) {
    let listener = match UnixListener::bind(path) {
        Ok(s) => s,
        Err(e) => {
            error!("Failed to create a Unix domain socket listener: {}", e);
            return;
        }
    };
    info!("Waiting for a GDB connection on {}...", path.display());

    let (stream, addr) = match listener.accept() {
        Ok(v) => v,
        Err(e) => {
            error!("Failed to accept a connection from GDB: {}", e);
            return;
        }
    };
    info!("GDB connected from {:?}", addr);

    let connection: Box<dyn ConnectionExt<Error = std::io::Error>> = Box::new(stream);
    let gdb = gdbstub::stub::GdbStub::new(connection);

    match gdb.run_blocking::<GdbEventLoop>(&mut gdbstub) {
        Ok(disconnect_reason) => match disconnect_reason {
            DisconnectReason::Disconnect => {
                info!("GDB client has disconnected. Running...");

                if let Err(e) = gdbstub.vm_request(GdbRequestPayload::SetSingleStep(false), 0) {
                    error!("Failed to disable single step: {:?}", e);
                }

                if let Err(e) =
                    gdbstub.vm_request(GdbRequestPayload::SetHwBreakPoint(Vec::new()), 0)
                {
                    error!("Failed to remove breakpoints: {:?}", e);
                }

                if let Err(e) = gdbstub.vm_request(GdbRequestPayload::Resume, 0) {
                    error!("Failed to resume the VM: {:?}", e);
                }
            }
            _ => {
                error!("Target exited or terminated");
            }
        },
        Err(e) => {
            error!("error occurred in GDB session: {}", e);
        }
    }
}
