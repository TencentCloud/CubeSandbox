// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use serde::{Deserialize, Serialize};

#[derive(Clone, Debug, Serialize, Deserialize, Default)]
pub struct PropagationMount {
    pub name: String,
}

#[derive(Clone, Debug, Serialize, Deserialize, Default)]
pub struct PropagationContainerMount {
    pub name: String,
    pub container_dir: String,
}
