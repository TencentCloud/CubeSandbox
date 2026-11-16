// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build linux

package storage

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// mountBDI returns the "major:minor" BDI name of the filesystem mounted at
// path (FUSE mounts get an anonymous 0:N device).
func mountBDI(path string) (string, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d", unix.Major(uint64(st.Dev)), unix.Minor(uint64(st.Dev))), nil
}
