// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build linux

package checkpoint

import (
	"os"

	"golang.org/x/sys/unix"
)

// cloneInto makes dst share every block of src (FICLONE). Both must be on
// the same reflink-capable filesystem.
func cloneInto(dst, src *os.File) error {
	return unix.IoctlFileClone(int(dst.Fd()), int(src.Fd()))
}

// punchHole turns [off, off+length) of f into a hole, which reads as zero.
func punchHole(f *os.File, off, length int64) error {
	return unix.Fallocate(int(f.Fd()), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, off, length)
}
