// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package mysql

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/dao"
)

func TestSessionLockerUsesConfiguredTimeout(t *testing.T) {
	d := &driver{}
	locker, ok := d.SessionLocker(dao.Config{MigrationLockTimeoutSeconds: 7}).(*sessionLocker)
	if !ok {
		t.Fatalf("SessionLocker type = %T, want *sessionLocker", d.SessionLocker(dao.Config{}))
	}
	if locker.timeout != 7 {
		t.Fatalf("timeout = %d, want 7", locker.timeout)
	}

	defaultLocker, ok := d.SessionLocker(dao.Config{}).(*sessionLocker)
	if !ok {
		t.Fatalf("SessionLocker type = %T, want *sessionLocker", d.SessionLocker(dao.Config{}))
	}
	if defaultLocker.timeout != defaultLockTimeoutSeconds {
		t.Fatalf("default timeout = %d, want %d", defaultLocker.timeout, defaultLockTimeoutSeconds)
	}
}
