// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//go:build unix && !linux

package content

import (
	"io/fs"
	"syscall"
	"time"
)

// statTimesOwner is only a development fallback: Cubelet runs on Linux, where
// stat_linux.go reports real access/change times.
func statTimesOwner(info fs.FileInfo) (atime, ctime time.Time, uid, gid uint32) {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return info.ModTime(), info.ModTime(), st.Uid, st.Gid
	}
	return info.ModTime(), info.ModTime(), 0, 0
}
