//! 文件域 wire 类型（含 watch 消息族）的 serde 形状。
//! 契约：spec/filesystem/filesystem.proto + 保真定制
//! （扁平 oneof / proto3 零值省略 / EntryInfo 形状——逐字节对拍的关键）。
//! 来源：承接 msg/filesystem.rs。
//! （PR-1 骨架：实现待 PR-2 搬运迁入）
