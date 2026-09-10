//! 生命周期端点：GET /health、POST /init、GET /envs。
//!
//! 回答：daemon 的存活探测、初始化与配置读取。
//! 来源：承接 rest/mod.rs 的对应 handler。
//! （PR-1 骨架：实现待 PR-2 自 rest/mod.rs 迁入）
