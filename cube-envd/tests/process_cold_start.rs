// SPDX-License-Identifier: Apache-2.0

use std::sync::Arc;

use cube_envd::error::DomainError;
use cube_envd::process::ProcessManager;
use cube_envd::runtime::{RuntimeStateStore, UserDatabase};
use cube_envd::server::LifecycleState;

#[tokio::test]
async fn cold_failed_start_does_not_retain_cgroup_or_descriptor() {
    // This separate test executable has no previously successful Start and
    // therefore no daemon-owned process resource class.
    let manager = Arc::new(ProcessManager::default());
    let runtime = RuntimeStateStore::new();
    let users = UserDatabase::system();
    tokio::fs::read("/etc/passwd").await.unwrap();
    tokio::fs::read("/etc/group").await.unwrap();
    let existed = std::path::Path::new("/sys/fs/cgroup/user").exists();
    let descriptors = || std::fs::read_dir("/proc/self/fd").unwrap().count();
    let before = descriptors();
    for _ in 0..3 {
        let request = serde_json::from_value(serde_json::json!({
            "process":{"cmd":"/bin/true","cwd":"/missing-cold-start-cwd"},"tag":"cold"
        }))
        .unwrap();
        assert!(matches!(
            manager
                .start(
                    request,
                    &http::HeaderMap::new(),
                    runtime.snapshot().await,
                    &users,
                    Arc::new(LifecycleState::new())
                )
                .await,
            Err(DomainError::InvalidArgument(_))
        ));
        assert!(manager.list().unwrap().processes.is_empty());
        assert_eq!(descriptors(), before, "failed Start retained a descriptor");
        assert_eq!(
            std::path::Path::new("/sys/fs/cgroup/user").exists(),
            existed
        );
    }
}
