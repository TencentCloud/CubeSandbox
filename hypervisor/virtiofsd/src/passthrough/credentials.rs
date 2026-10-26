// SPDX-License-Identifier: BSD-3-Clause

use crate::oslib;
use crate::passthrough::util::einval;
use std::io;

pub struct UnixCredentials {
    uid: libc::uid_t,
    gid: libc::gid_t,
    sup_gid: Option<u32>,
    keep_capability: bool,
}

impl UnixCredentials {
    pub fn new(uid: libc::uid_t, gid: libc::gid_t) -> Self {
        UnixCredentials {
            uid,
            gid,
            sup_gid: None,
            keep_capability: false,
        }
    }

    /// Set a supplementary group. Set `supported_extension` to `false` to signal that a
    /// supplementary group maybe required, but the guest was not able to tell us which,
    /// so we have to rely on keeping the DAC_OVERRIDE capability.
    pub fn supplementary_gid(self, supported_extension: bool, sup_gid: Option<u32>) -> Self {
        UnixCredentials {
            uid: self.uid,
            gid: self.gid,
            sup_gid,
            keep_capability: !supported_extension,
        }
    }

    /// Changes the effective uid/gid of the current thread to `val`.  Changes
    /// the thread's credentials back to root when the returned struct is dropped.
    pub fn set(self) -> io::Result<Option<UnixCredentialsGuard>> {
        let change_uid = self.uid != 0;
        let change_gid = self.gid != 0;

        // We have to change the gid before we change the uid because if we
        // change the uid first then we lose the capability to change the gid.
        // However changing back can happen in any order.
        if let Some(sup_gid) = self.sup_gid {
            oslib::setsupgroup(sup_gid)?;
        }

        if change_gid {
            oslib::seteffgid(self.gid)?;
        }

        if change_uid {
            oslib::seteffuid(self.uid)?;
        }

        if change_uid && self.keep_capability {
            // Before kernel 6.3, we don't have access to process supplementary groups.
            // To work around this we can set the `DAC_OVERRIDE` in the effective set.
            // We are allowed to set the capability because we only change the effective
            // user ID, so we still have the 'DAC_OVERRIDE' in the permitted set.
            // After switching back to root the permitted set is copied to the effective set,
            // so no additional steps are required.
            if let Err(e) = crate::util::add_cap_to_eff("DAC_OVERRIDE") {
                warn!("failed to add 'DAC_OVERRIDE' to the effective set of capabilities: {e}");
            }
        }

        if !change_uid && !change_gid {
            return Ok(None);
        }

        Ok(Some(UnixCredentialsGuard {
            reset_uid: change_uid,
            reset_gid: change_gid,
            drop_sup_gid: self.sup_gid.is_some(),
        }))
    }
}

pub struct UnixCredentialsGuard {
    reset_uid: bool,
    reset_gid: bool,
    drop_sup_gid: bool,
}

impl Drop for UnixCredentialsGuard {
    fn drop(&mut self) {
        if self.reset_uid {
            oslib::seteffuid(0).unwrap_or_else(|e| {
                error!("failed to change uid back to root: {e}");
            });
        }

        if self.reset_gid {
            oslib::seteffgid(0).unwrap_or_else(|e| {
                error!("failed to change gid back to root: {e}");
            });
        }

        if self.drop_sup_gid {
            oslib::dropsupgroups().unwrap_or_else(|e| {
                error!("failed to drop supplementary groups: {e}");
            });
        }
    }
}

pub struct ScopedCaps {
    cap: capng::Capability,
}

impl ScopedCaps {
    fn new(cap_name: &str) -> io::Result<Option<Self>> {
        use capng::{Action, CUpdate, Set, Type};

        let cap = capng::name_to_capability(cap_name).map_err(|_| {
            let err = io::Error::last_os_error();
            error!(
                "couldn't get the capability id for name {}: {:?}",
                cap_name, err
            );
            err
        })?;

        if capng::have_capability(Type::EFFECTIVE, cap) {
            let req = vec![CUpdate {
                action: Action::DROP,
                cap_type: Type::EFFECTIVE,
                capability: cap,
            }];
            capng::update(req).map_err(|e| {
                error!("couldn't drop {} capability: {:?}", cap, e);
                einval()
            })?;
            capng::apply(Set::CAPS).map_err(|e| {
                error!(
                    "couldn't apply capabilities after dropping {}: {:?}",
                    cap, e
                );
                einval()
            })?;
            Ok(Some(Self { cap }))
        } else {
            Ok(None)
        }
    }
}

impl Drop for ScopedCaps {
    fn drop(&mut self) {
        use capng::{Action, CUpdate, Set, Type};

        let req = vec![CUpdate {
            action: Action::ADD,
            cap_type: Type::EFFECTIVE,
            capability: self.cap,
        }];

        if let Err(e) = capng::update(req) {
            panic!("couldn't restore {} capability: {:?}", self.cap, e);
        }
        if let Err(e) = capng::apply(Set::CAPS) {
            panic!(
                "couldn't apply capabilities after restoring {}: {:?}",
                self.cap, e
            );
        }
    }
}

pub fn drop_effective_cap(cap_name: &str) -> io::Result<Option<ScopedCaps>> {
    ScopedCaps::new(cap_name)
}
