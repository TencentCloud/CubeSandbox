//! 传输层 handler 管道：`unary_endpoint<T, F>` 泛型流水线
//! （token 检查 → 请求解析 → 用户解析 → legacy 判定 → 阻塞池穿越 → 响应组装）。
//!
//! 回答：一个 Connect unary 请求从字节到响应要经过哪几步。
//! 来源：承接 server.rs 的 `fs_unary_endpoint` 泛型函数（#18）与
//! `watch_unary!` 宏（registry 归属定论后一并收编）。
//! （PR-1 骨架：实现待 PR-2 自 server.rs 迁入）
