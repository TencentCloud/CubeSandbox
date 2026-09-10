//! 唯一接线表：daemon 暴露的每一个 endpoint 在此注册，一行一个。
//!
//! 回答：这个 daemon 对外暴露哪些接口、各自对应哪个 handler。
//! 约定：本文件只做接线——域逻辑在 filesystem/ 与 process/，
//! 传输管道在 handlers.rs（unary_endpoint 泛型流水线）。
//! 来源：承接 server.rs 的 `.route(...)` 注册表（23 条）。
//! （PR-1 骨架：实现待 PR-2 自 server.rs 迁入）
