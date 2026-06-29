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

// sessionLocker implements goose.SessionLocker on top of PostgreSQL's
// pg_advisory_lock / pg_advisory_unlock. Advisory locks are session-scoped:
// when the connection goes away (process crash, broken pipe), PostgreSQL
// releases the lock automatically — there is no need for a janitor / TTL.
type sessionLocker struct {
	id      int64
	timeout int // seconds
}

// SessionLock acquires a session-level advisory lock with a timeout.
// PostgreSQL's pg_advisory_lock blocks indefinitely, so we use
// pg_try_advisory_lock in a retry loop with a deadline derived from
// s.timeout. This mirrors the MySQL driver's GET_LOCK(name, timeout)
// semantics. Retries use exponential backoff (200ms → 400ms → … capped
// at 2s) to avoid thundering-herd pressure on cluster restart.
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
		// Exponential backoff capped at maxInterval.
		interval = interval * 2
		if interval > maxInterval {
			interval = maxInterval
		}
	}
}

// SessionUnlock is best-effort: if the lock has already been released by
// connection death there is nothing to do, and surfacing such errors would
// mask the real (preceding) failure.
func (s *sessionLocker) SessionUnlock(ctx context.Context, conn *sql.Conn) error {
	_, err := conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", s.id)
	return err
}
