// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use std::{collections::BTreeMap, sync::RwLock};

use serde::Deserialize;

#[derive(Default)]
/// 以读写锁保护由 init API 管理的环境变量集合。
///
/// 语义与参考实现一致（`packages/envd/main.go:154-159`、`internal/api/init.go:189-196`）：
/// 集合在启动时种入 `E2B_SANDBOX`，之后由 `/init` **逐键合并**，而不是整体替换。
/// 这一点是可观察的：模板镜像自带的环境变量、控制面多次 /init（例如快照恢复后再注入）
/// 都必须叠加而不是互相覆盖。
pub struct Environment {
    /// 保存当前完整环境变量集合。
    variables: RwLock<BTreeMap<String, String>>,
}

/// 提供环境变量集合的初始化、合并和读取操作。
impl Environment {
    /// 按参考实现的初始状态创建集合：只含 `E2B_SANDBOX`。
    ///
    /// 参考实现写入 `strconv.FormatBool(!isNotFC)`；cube-envd 没有 Firecracker 分支，
    /// 因此这里直接沿用入口脚本固定传入的 `-isnotfc` 语义（FC 关闭 → `false`）。
    pub fn new() -> Self {
        let mut variables = BTreeMap::new();
        variables.insert("E2B_SANDBOX".to_owned(), "false".to_owned());
        Self {
            variables: RwLock::new(variables),
        }
    }

    /// 逐键合并新提交的环境变量，保留未提及的既有取值。
    pub fn merge(&self, variables: BTreeMap<String, String>) {
        let mut current = self.variables.write().expect("environment lock poisoned");
        for (key, value) in variables {
            current.insert(key, value);
        }
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
