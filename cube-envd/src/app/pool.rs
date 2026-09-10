//! 阻塞池策略：每次请求一次池穿越，绝不按 syscall 穿越（~29µs/次）。
//!
//! 回答：阻塞操作在哪个池、以什么粒度执行。
//! 消费方：filesystem RPC（unary_endpoint）/ 上传写任务 / watch / metrics 采样。
//! 来源：承接 blocking.rs（原样迁入）。
//! （PR-1 骨架：实现待 PR-2 搬运迁入）
