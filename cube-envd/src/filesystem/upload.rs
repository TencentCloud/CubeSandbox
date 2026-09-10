//! 数据面：客户端字节怎么落盘——原地 O_TRUNC 流式写 + multipart 泵。
//! 契约：上游 upload.go:68（os.OpenFile 跟随符号链接）+ os.Chown（跟随）
//! + 256 MiB 计数闸（413 中停，部分内容留存 = 中断写入语义）。
//! 来源：承接 rest/files.rs 上传半（PR-C）。
//! （PR-1 骨架：实现待 PR-2 搬运迁入）
