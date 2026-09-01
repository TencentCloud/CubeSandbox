use std::{collections::BTreeMap, sync::RwLock};

use serde::Deserialize;

#[derive(Default)]
/// 以读写锁保护由 init API 管理的环境变量快照。
pub struct Environment {
    /// 保存当前完整环境变量集合。
    variables: RwLock<BTreeMap<String, String>>,
}

/// 提供环境变量快照的替换和读取操作。
impl Environment {
    /// 用新集合整体替换当前环境变量。
    pub fn replace(&self, variables: BTreeMap<String, String>) {
        *self.variables.write().expect("environment lock poisoned") = variables;
    }

    /// 返回当前环境变量的独立副本。
    pub fn snapshot(&self) -> BTreeMap<String, String> {
        self.variables
            .read()
            .expect("environment lock poisoned")
            .clone()
    }
}

#[derive(Default, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
/// 表示 /init 接受的严格 JSON 请求体。
pub struct InitRequest {
    /// 可选的完整环境变量集合；缺失时保留原快照。
    pub env_vars: Option<BTreeMap<String, String>>,
}
