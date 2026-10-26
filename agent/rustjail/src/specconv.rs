// Copyright (c) 2019 Ant Financial
//
// SPDX-License-Identifier: Apache-2.0
//

use oci::Spec;

#[derive(Serialize, Deserialize, Debug, Default, Clone)]
pub struct CreateOpts {
    pub cgroup_name: String,
    pub use_systemd_cgroup: bool,
    pub no_pivot_root: bool,
    pub no_new_keyring: bool,
    pub spec: Option<Spec>,
    pub rootless_euid: bool,
    pub rootless_cgroup: bool,
}
