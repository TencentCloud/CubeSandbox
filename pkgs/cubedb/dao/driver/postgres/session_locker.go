// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// sessionLocker implements goose.SessionLocker via pg_advisory_lock.
// Session-scoped: connection death releases the lock (no janitor/TTL).
type sessionLocker struct {
	id      int64
	timeout int // seconds
}

// SessionLock uses pg_try_advisory_lock + retry until s.timeout
// (GET_LOCK-compatible); backoff 200ms→2s.
func (s *sessionLocker) SessionLock(ctx context.Context, conn *sql.Conn) error {
	deadline := time.Now().Add(time.Duration(s.timeout) * time.Second)
	const (
		initialInterval = 200 * time.Millisecond
		maxInterval     = 2 * time.Second
	)
	interval := initialInterval

	for {
		var acquired bool
		if err := conn.QueryRowContext(ctx,
			"SELECT pg_try_advisory_lock($1)", s.id).Scan(&acquired); err != nil {
			return fmt.Errorf("acquire advisory lock %d: %w", s.id, err)
		}
		if acquired {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("acquire advisory lock %d: timeout after %ds", s.id, s.timeout)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("acquire advisory lock %d: %w", s.id, ctx.Err())
		case <-time.After(interval):
		}
		interval = interval * 2
		if interval > maxInterval {
			interval = maxInterval
		}
	}
}

// SessionUnlock is best-effort so unlock errors do not mask a prior failure.
func (s *sessionLocker) SessionUnlock(ctx context.Context, conn *sql.Conn) error {
	_, err := conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", s.id)
	return err
}
