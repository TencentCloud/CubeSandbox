//! 事件怎么变成语义：展开次序 / 递归 / cookie 配对 / 目录映射 / MOVE_SELF。
//! 契约：e2b fsnotify backend_inotify.go:568-596 等（逐条核对）。
//! 来源：承接 services/watch.rs 的语义机（P1 修复所在层）。
//! （PR-1 骨架：实现待 PR-2 搬运迁入）
