// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package content

import (
	"io/fs"
	"syscall"
	"time"
)

func statTimesOwner(info fs.FileInfo) (atime, ctime time.Time, uid, gid uint32) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return info.ModTime(), info.ModTime(), 0, 0
	}
	return time.Unix(st.Atim.Sec, st.Atim.Nsec), time.Unix(st.Ctim.Sec, st.Ctim.Nsec), st.Uid, st.Gid
}
