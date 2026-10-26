// Copyright © 2023 Tencent Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

use event_notifier::{event_notify, NotifyEvent};
use serde::{Deserialize, Serialize};
use vm_device::BusDevice;
use vm_migration::{Migratable, MigratableError, Pausable, Snapshot, Snapshottable, Transportable};

/// Debug I/O port, see:
/// https://bochs.sourceforge.io/techspec/PORTS.LST
/// https://wiki.osdev.org/I/O_Ports
///
/// Here we use 0x0680 as system control port address.

const SYS_START: u8 = 1 << 0;
const SYS_RESTORE: u8 = 1 << 1;
const SYS_PANIC: u8 = 1 << 2;
const SYS_VSOCK_SERVER: u8 = 1 << 3;
const SYS_VALID: u8 = SYS_START | SYS_RESTORE;

fn sys_start(sys_state: u8) -> bool {
    (sys_state & SYS_START) == SYS_START
}

#[derive(Serialize, Deserialize)]
pub struct SysCtrlState {
    state: u8,
}

pub struct SysCtrl {
    id: String,
    sys_state: u8,
    sys_started: std::sync::Once,
}

impl SysCtrl {
    pub fn new(id: String, state: Option<SysCtrlState>) -> Self {
        let mut sys_state = 0;
        if let Some(state) = state {
            sys_state = state.state;
        }

        Self {
            id,
            sys_state,
            sys_started: std::sync::Once::new(),
        }
    }

    pub fn sys_started(&self) -> bool {
        sys_start(self.sys_state)
    }

    fn state(&self) -> SysCtrlState {
        SysCtrlState {
            state: self.sys_state,
        }
    }
}

impl BusDevice for SysCtrl {
    fn read(&mut self, _base: u64, _offset: u64, data: &mut [u8]) {
        self.sys_started
            .call_once(|| info!("read system control state {}", self.sys_state));
        data[0] = self.sys_state & SYS_VALID;
    }

    fn write(
        &mut self,
        _base: u64,
        _offset: u64,
        data: &[u8],
    ) -> Option<std::sync::Arc<std::sync::Barrier>> {
        let code = data[0];

        if code == self.sys_state {
            return None;
        }

        debug!(
            "write system control from {:x} into {:x}",
            self.sys_state, code
        );
        if sys_start(code) && !sys_start(self.sys_state) {
            self.sys_state |= SYS_START;
            event_notify!(NotifyEvent::SysStart);
        }

        if (code & SYS_PANIC) == SYS_PANIC {
            warn!("Guest paniced and coredump");
        }
        if (code & SYS_VSOCK_SERVER) == SYS_VSOCK_SERVER {
            info!("vsock server ready");
            event_notify!(NotifyEvent::VsockServerReady);
        }

        None
    }
}

impl Pausable for SysCtrl {}

impl Snapshottable for SysCtrl {
    fn id(&self) -> String {
        self.id.clone()
    }

    fn snapshot(&mut self) -> std::result::Result<Snapshot, MigratableError> {
        let snapshot = Snapshot::new_from_state(&self.id, &self.state())?;

        Ok(snapshot)
    }

    fn restore(&mut self, _: Snapshot) -> std::result::Result<(), MigratableError> {
        self.sys_state |= SYS_RESTORE;
        Ok(())
    }
}

impl Transportable for SysCtrl {}
impl Migratable for SysCtrl {}
