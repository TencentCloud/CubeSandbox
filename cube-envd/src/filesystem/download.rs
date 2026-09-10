//! 数据面：Range / 条件请求 / Content-Type 嗅探 / 64KiB 流式读。
//! 契约：上游 api/download.go + item 1.3（8-stage 判定链）。
//! 来源：承接 rest/files.rs 下载半。
//! （PR-1 骨架：实现待 PR-2 搬运迁入）
