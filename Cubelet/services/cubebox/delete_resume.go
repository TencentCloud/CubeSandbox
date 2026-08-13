// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"time"
)

const deleteLifecycleLockMaxWait = 2 * time.Second

// deleteLifecycleLockDeadline bounds how long Destroy waits for the per-sandbox
// lifecycle lock so the Cubelet RPC still has time to return.
func deleteLifecycleLockDeadline(ctx context.Context, now time.Time) (time.Time, bool) {
	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		return now.Add(deleteLifecycleLockMaxWait), true
	}
	if !deadline.After(now) {
		return time.Time{}, false
	}
	maxLockDeadline := now.Add(deleteLifecycleLockMaxWait)
	if deadline.Before(maxLockDeadline) {
		// Leave a tiny floor for the RPC response after the lock attempt.
		latest := deadline.Add(-100 * time.Millisecond)
		if !latest.After(now) {
			return time.Time{}, false
		}
		return latest, true
	}
	return maxLockDeadline, true
}
