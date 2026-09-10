//! 运行期配置：默认用户/工作目录、env 变量、init 时间戳闸门。
//!
//! 回答：/init 写入了什么默认值、后续请求如何消费它们。
//! 来源：承接 state.rs 的 merge_env_vars / env_vars / default_user /
//! default_workdir / apply_init_defaults / claim_timestamp。
//! （PR-1 骨架：实现待 PR-2 自 state.rs 迁入）
