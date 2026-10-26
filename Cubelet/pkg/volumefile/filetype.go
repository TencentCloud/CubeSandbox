// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package volumefile

type FileType uint32

const (
	FtCode     FileType = 0
	FtLayer    FileType = 1
	FtLang     FileType = 2
	FtLangExt4 FileType = 3
)
