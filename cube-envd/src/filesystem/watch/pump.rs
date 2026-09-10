//! 流怎么活怎么死：deadline / keepalive / 断连四路 select。
//! 契约：上游 watch.go:66-90。
//! 来源：承接 services/watch.rs 的流式泵。
//! （PR-1 骨架：实现待 PR-2 搬运迁入）
