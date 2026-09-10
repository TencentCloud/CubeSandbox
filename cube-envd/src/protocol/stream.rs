//! 共享流式投递：响应队列占槽 / producer 永不等容量 / drop body 唤醒。
//! 回答：流式响应怎么投递、背压与断连怎么处理。
//! 来源：承接 connect.rs 流式投递部分；重构合入后对齐
//! refactor 分支的 connect/stream.rs（两版取一）。
//! （PR-1 骨架：实现待 PR-2 搬运迁入）
