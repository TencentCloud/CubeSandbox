//! AppState 组合根：把进程表、配置、令牌、cgroup 句柄装配成一个共享容器。
//!
//! 回答：daemon 运行期共享了什么状态、由谁持有。
//! 拆分：本文件瘦身为组合根；进程表在 process/table.rs，
//! 配置在 config.rs，令牌在 token.rs，cgroup 句柄归 cgroup/。
//! 来源：承接 state.rs（664 行四类职责混装，复审确认拆分）。
//! （PR-1 骨架：实现待 PR-2 搬运迁入）
