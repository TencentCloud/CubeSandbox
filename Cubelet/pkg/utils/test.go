// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func SkipCI(t testing.TB) {
	if os.Getenv("CI") != "" {
		t.Skip("Skipping testing in CI environment")
	}
}

// SkipUnlessRootWithSysAdmin skips tests unless they run as root with effective
// CAP_SYS_ADMIN. Requiring both preserves the existing contract of tests that
// format filesystems or mount images, even if a non-root process has the cap.
func SkipUnlessRootWithSysAdmin(t testing.TB) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("skipping test that requires root with CAP_SYS_ADMIN")
	}

	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := [2]unix.CapUserData{}
	if err := unix.Capget(&header, &data[0]); err != nil {
		t.Skipf("skipping test: failed to query effective capabilities: %v", err)
	}
	if data[unix.CAP_SYS_ADMIN/32].Effective&(1<<uint(unix.CAP_SYS_ADMIN%32)) == 0 {
		t.Skip("skipping test that requires root with CAP_SYS_ADMIN")
	}
}
