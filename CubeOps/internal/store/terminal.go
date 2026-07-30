// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var (
	ErrTerminalGrantInvalid = errors.New("terminal grant is invalid")
	ErrTerminalGrantLimit   = errors.New("terminal pending grant limit exceeded")
	ErrTerminalSessionLost  = errors.New("terminal session is unavailable")
	ErrTerminalSessionLimit = errors.New("terminal active session limit exceeded")
)

// TerminalGrant is the persisted, target-bound form of a one-time browser
// credential. TokenHash is a SHA-256 hex digest; the raw grant is never stored.
type TerminalGrant struct {
	ID           string
	TokenHash    string
	Kind         string
	UserID       string
	SandboxID    string
	ContainerID  string
	SessionID    string
	Cols         uint32
	Rows         uint32
	ResumeOffset int64
	CreatedAt    time.Time
	ExpiresAt    time.Time
	ConsumedAt   sql.NullTime
}

// TerminalSession is both the active-session registry and the payload-free
// terminal audit record.
type TerminalSession struct {
	ID          string
	UserID      string
	SandboxID   string
	ContainerID string
	CubeletHost string
	OpenedAt    time.Time
	LastSeenAt  time.Time
	ClosedAt    sql.NullTime
	CloseReason sql.NullString
	ExitCode    sql.NullInt64
	BytesIn     int64
	BytesOut    int64
	ResumeCount int
}

// CreateTerminalGrant serializes issuance per platform user by locking the
// corresponding t_system_user row. The count and insert therefore remain an
// exact multi-replica limit rather than a process-local approximation.
func (s *Store) CreateTerminalGrant(ctx context.Context, grant *TerminalGrant, maxPending int) error {
	if grant == nil || maxPending <= 0 {
		return errors.New("terminal grant configuration is invalid")
	}
	return s.terminalGrantDB(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockTerminalUser(tx, grant.UserID); err != nil {
			return err
		}
		var pending int64
		if err := tx.Raw(`SELECT COUNT(*) FROM terminal_grants
			WHERE user_id = ? AND consumed_at IS NULL AND expires_at > CURRENT_TIMESTAMP`, grant.UserID).
			Scan(&pending).Error; err != nil {
			return fmt.Errorf("count pending terminal grants: %w", err)
		}
		if pending >= int64(maxPending) {
			return ErrTerminalGrantLimit
		}

		var sessionID any
		if grant.SessionID != "" {
			sessionID = grant.SessionID
		}
		colsColumn, rowsColumn := terminalDimensionColumns(tx)
		result := tx.Exec(fmt.Sprintf(`INSERT INTO terminal_grants
			(id, token_hash, kind, user_id, sandbox_id, container_id, session_id,
			 %s, %s, resume_offset, created_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, colsColumn, rowsColumn),
			grant.ID, grant.TokenHash, grant.Kind, grant.UserID, grant.SandboxID,
			grant.ContainerID, sessionID, grant.Cols, grant.Rows, grant.ResumeOffset,
			grant.CreatedAt, grant.ExpiresAt)
		if result.Error != nil {
			return fmt.Errorf("create terminal grant: %w", result.Error)
		}
		return nil
	})
}

// ConsumeTerminalGrant atomically marks an unexpired grant consumed. Exactly
// one concurrent caller can observe RowsAffected==1; every other case is the
// same non-oracular ErrTerminalGrantInvalid result.
func (s *Store) ConsumeTerminalGrant(ctx context.Context, tokenHash string) (*TerminalGrant, error) {
	result := s.terminalGrantDB(ctx).Exec(`UPDATE terminal_grants
		SET consumed_at = CURRENT_TIMESTAMP
		WHERE token_hash = ? AND consumed_at IS NULL AND expires_at > CURRENT_TIMESTAMP`, tokenHash)
	if result.Error != nil {
		return nil, fmt.Errorf("consume terminal grant: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return nil, ErrTerminalGrantInvalid
	}

	grant, err := s.getTerminalGrantByHash(ctx, tokenHash)
	if err != nil {
		return nil, err
	}
	return grant, nil
}

func (s *Store) getTerminalGrantByHash(ctx context.Context, tokenHash string) (*TerminalGrant, error) {
	var grant TerminalGrant
	colsColumn, rowsColumn := terminalDimensionColumns(s.db)
	result := s.terminalGrantDB(ctx).Raw(fmt.Sprintf(`SELECT id, token_hash, kind, user_id,
			sandbox_id, container_id, COALESCE(session_id, '') AS session_id,
			%s, %s, resume_offset, created_at, expires_at, consumed_at
			FROM terminal_grants WHERE token_hash = ? LIMIT 1`, colsColumn, rowsColumn), tokenHash).Scan(&grant)
	if result.Error != nil {
		return nil, fmt.Errorf("read consumed terminal grant: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return nil, ErrTerminalGrantInvalid
	}
	return &grant, nil
}

// CleanupTerminalGrants removes only old expired/consumed authorization
// records. Recent rows remain available for security audit queries.
func (s *Store) CleanupTerminalGrants(ctx context.Context, before time.Time) (int64, error) {
	result := s.terminalGrantDB(ctx).Exec("DELETE FROM terminal_grants WHERE expires_at < ?", before)
	if result.Error != nil {
		return 0, fmt.Errorf("cleanup terminal grants: %w", result.Error)
	}
	return result.RowsAffected, nil
}

// GetTerminalSession returns a session regardless of open/closed state so the
// service can apply user binding and resume policy without exposing row
// existence to the browser.
func (s *Store) GetTerminalSession(ctx context.Context, sessionID string) (*TerminalSession, error) {
	var session TerminalSession
	result := s.db.WithContext(ctx).Raw(`SELECT id, user_id, sandbox_id,
		container_id, cubelet_host, opened_at, last_seen_at, closed_at,
		close_reason, exit_code, bytes_in, bytes_out, resume_count
		FROM terminal_sessions WHERE id = ? LIMIT 1`, sessionID).Scan(&session)
	if result.Error != nil {
		return nil, fmt.Errorf("read terminal session: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return nil, ErrTerminalSessionLost
	}
	return &session, nil
}

// CreateTerminalSession inserts the pre-open audit row while holding the same
// per-user database row lock used by grant issuance. This makes the active
// session limit exact across CubeOps replicas.
func (s *Store) CreateTerminalSession(ctx context.Context, session *TerminalSession, maxActive int, activeSince time.Time) error {
	if session == nil || maxActive <= 0 {
		return errors.New("terminal session configuration is invalid")
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockTerminalUser(tx, session.UserID); err != nil {
			return err
		}
		var active int64
		if err := tx.Raw(`SELECT COUNT(*) FROM terminal_sessions
			WHERE user_id = ? AND closed_at IS NULL AND last_seen_at > ?`, session.UserID, activeSince).
			Scan(&active).Error; err != nil {
			return fmt.Errorf("count active terminal sessions: %w", err)
		}
		if active >= int64(maxActive) {
			return ErrTerminalSessionLimit
		}
		result := tx.Exec(`INSERT INTO terminal_sessions
			(id, user_id, sandbox_id, container_id, cubelet_host, opened_at, last_seen_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, session.ID, session.UserID, session.SandboxID,
			session.ContainerID, session.CubeletHost, session.OpenedAt, session.LastSeenAt)
		if result.Error != nil {
			return fmt.Errorf("create terminal session: %w", result.Error)
		}
		return nil
	})
}

// ResumeTerminalSession verifies the original user and target binding and
// atomically reactivates only a still-open detached session.
func (s *Store) ResumeTerminalSession(ctx context.Context, sessionID, userID, sandboxID, containerID string, seenAt, activeSince time.Time) error {
	result := s.db.WithContext(ctx).Exec(`UPDATE terminal_sessions
		SET last_seen_at = ?, resume_count = resume_count + 1
		WHERE id = ? AND user_id = ? AND sandbox_id = ? AND container_id = ?
		  AND closed_at IS NULL AND last_seen_at > ?`,
		seenAt, sessionID, userID, sandboxID, containerID, activeSince)
	if result.Error != nil {
		return fmt.Errorf("resume terminal session: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return ErrTerminalSessionLost
	}
	return nil
}

// TouchTerminalSession writes absolute byte counters. Absolute values make a
// retry idempotent and avoid double-counting a heartbeat after a transient DB
// error.
func (s *Store) TouchTerminalSession(ctx context.Context, sessionID string, seenAt time.Time, bytesIn, bytesOut int64) error {
	result := s.db.WithContext(ctx).Exec(`UPDATE terminal_sessions
		SET last_seen_at = ?, bytes_in = GREATEST(bytes_in, ?), bytes_out = GREATEST(bytes_out, ?)
		WHERE id = ? AND closed_at IS NULL`, seenAt, bytesIn, bytesOut, sessionID)
	if result.Error != nil {
		return fmt.Errorf("touch terminal session: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return ErrTerminalSessionLost
	}
	return nil
}

// CloseTerminalSession finalizes the audit row without terminal payload. The
// COALESCE/GREATEST assignments keep repeated close attempts idempotent while
// still allowing a later retry to fill an exit code that arrived just before
// transport teardown.
func (s *Store) CloseTerminalSession(ctx context.Context, sessionID string, closedAt time.Time, reason string, exitCode *int32, bytesIn, bytesOut int64) error {
	var nullableExit any
	if exitCode != nil {
		nullableExit = *exitCode
	}
	result := s.db.WithContext(ctx).Exec(`UPDATE terminal_sessions
		SET last_seen_at = ?, bytes_in = GREATEST(bytes_in, ?), bytes_out = GREATEST(bytes_out, ?),
		    close_reason = COALESCE(close_reason, ?), exit_code = COALESCE(exit_code, ?),
		    closed_at = COALESCE(closed_at, ?)
		WHERE id = ?`, closedAt, bytesIn, bytesOut, reason, nullableExit, closedAt, sessionID)
	if result.Error != nil {
		return fmt.Errorf("close terminal session: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return ErrTerminalSessionLost
	}
	return nil
}

// ReconcileOrphanTerminalSessions closes registry rows whose owning CubeOps
// replica stopped heartbeating without a final audit update.
func (s *Store) ReconcileOrphanTerminalSessions(ctx context.Context, staleBefore, closedAt time.Time) (int64, error) {
	result := s.db.WithContext(ctx).Exec(`UPDATE terminal_sessions
		SET closed_at = ?, close_reason = COALESCE(close_reason, 'ORPHANED')
		WHERE closed_at IS NULL AND last_seen_at < ?`, closedAt, staleBefore)
	if result.Error != nil {
		return 0, fmt.Errorf("reconcile orphan terminal sessions: %w", result.Error)
	}
	return result.RowsAffected, nil
}

func lockTerminalUser(tx *gorm.DB, userID string) error {
	var lockedUser string
	err := tx.Raw("SELECT username FROM t_system_user WHERE username = ? FOR UPDATE", userID).
		Row().Scan(&lockedUser)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("terminal user no longer exists")
	}
	if err != nil {
		return fmt.Errorf("lock terminal user: %w", err)
	}
	return nil
}

func terminalDimensionColumns(db *gorm.DB) (string, string) {
	return db.Statement.Quote("cols"), db.Statement.Quote("rows")
}

func (s *Store) terminalGrantDB(ctx context.Context) *gorm.DB {
	// GORM's default slow-query logger interpolates bound values. Grant rows
	// contain the SHA-256 verifier for a browser credential, so even their SQL
	// parameters must never reach application logs. Callers still receive and
	// classify database errors; only SQL statement logging is suppressed.
	return s.db.Session(&gorm.Session{Logger: logger.Discard}).WithContext(ctx)
}
