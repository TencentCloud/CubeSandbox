// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
	gormlogger "gorm.io/gorm/logger"
)

type captureTerminalSQLLogger struct {
	mu         sync.Mutex
	statements []string
}

func (l *captureTerminalSQLLogger) LogMode(gormlogger.LogLevel) gormlogger.Interface { return l }
func (l *captureTerminalSQLLogger) Info(context.Context, string, ...any)             {}
func (l *captureTerminalSQLLogger) Warn(context.Context, string, ...any)             {}
func (l *captureTerminalSQLLogger) Error(context.Context, string, ...any)            {}
func (l *captureTerminalSQLLogger) Trace(_ context.Context, _ time.Time, query func() (string, int64), _ error) {
	statement, _ := query()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.statements = append(l.statements, statement)
}

func (l *captureTerminalSQLLogger) contains(value string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Contains(strings.Join(l.statements, "\n"), value)
}

func TestTerminalStoreGrantAndSessionSemantics(t *testing.T) {
	env := newTestStore(t)
	defer env.teardown()
	s := env.store
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	if err := s.DB().WithContext(ctx).Exec("DELETE FROM terminal_sessions").Error; err != nil {
		t.Fatalf("clear terminal_sessions: %v", err)
	}
	if err := s.DB().WithContext(ctx).Exec("DELETE FROM terminal_grants").Error; err != nil {
		t.Fatalf("clear terminal_grants: %v", err)
	}

	t.Run("grant SQL logging omits token verifier", func(t *testing.T) {
		capture := &captureTerminalSQLLogger{}
		db := s.DB()
		previous := db.Logger
		db.Logger = capture
		defer func() { db.Logger = previous }()

		grant := terminalGrant("sql-log-verifier", now, now.Add(time.Minute))
		if err := s.CreateTerminalGrant(ctx, grant, 10); err != nil {
			t.Fatalf("CreateTerminalGrant: %v", err)
		}
		if _, err := s.ConsumeTerminalGrant(ctx, grant.TokenHash); err != nil {
			t.Fatalf("ConsumeTerminalGrant: %v", err)
		}
		if capture.contains(grant.TokenHash) {
			t.Fatal("terminal grant verifier reached the SQL logger")
		}
	})

	t.Run("pending limit ignores expired grants", func(t *testing.T) {
		if err := s.CreateTerminalGrant(ctx, terminalGrant("expired", now, now.Add(-time.Minute)), 2); err != nil {
			t.Fatalf("create expired grant: %v", err)
		}
		if err := s.CreateTerminalGrant(ctx, terminalGrant("pending-1", now, now.Add(time.Minute)), 2); err != nil {
			t.Fatalf("create pending grant 1: %v", err)
		}
		if err := s.CreateTerminalGrant(ctx, terminalGrant("pending-2", now, now.Add(time.Minute)), 2); err != nil {
			t.Fatalf("create pending grant 2: %v", err)
		}
		if err := s.CreateTerminalGrant(ctx, terminalGrant("pending-3", now, now.Add(time.Minute)), 2); !errors.Is(err, store.ErrTerminalGrantLimit) {
			t.Fatalf("third pending grant error = %v, want ErrTerminalGrantLimit", err)
		}
	})

	if err := s.DB().WithContext(ctx).Exec("DELETE FROM terminal_grants").Error; err != nil {
		t.Fatalf("reset terminal_grants: %v", err)
	}

	t.Run("single-use consume is atomic", func(t *testing.T) {
		grant := terminalGrant("single-use", now, now.Add(time.Minute))
		if err := s.CreateTerminalGrant(ctx, grant, 10); err != nil {
			t.Fatalf("CreateTerminalGrant: %v", err)
		}

		const consumers = 16
		var successCount atomic.Int32
		errorsCh := make(chan error, consumers)
		var wg sync.WaitGroup
		wg.Add(consumers)
		for range consumers {
			go func() {
				defer wg.Done()
				consumed, err := s.ConsumeTerminalGrant(ctx, grant.TokenHash)
				if err == nil {
					if consumed.ID != grant.ID || consumed.SessionID != grant.SessionID {
						errorsCh <- fmt.Errorf("consumed binding = %+v", consumed)
						return
					}
					successCount.Add(1)
					return
				}
				if !errors.Is(err, store.ErrTerminalGrantInvalid) {
					errorsCh <- err
				}
			}()
		}
		wg.Wait()
		close(errorsCh)
		for err := range errorsCh {
			t.Errorf("concurrent consume: %v", err)
		}
		if successCount.Load() != 1 {
			t.Fatalf("successful consumers = %d, want 1", successCount.Load())
		}
		if _, err := s.ConsumeTerminalGrant(ctx, grant.TokenHash); !errors.Is(err, store.ErrTerminalGrantInvalid) {
			t.Fatalf("replayed consume error = %v", err)
		}
	})

	t.Run("expired grant and cleanup", func(t *testing.T) {
		grant := terminalGrant("expired-consume", now.Add(-2*time.Minute), now.Add(-time.Minute))
		if err := s.CreateTerminalGrant(ctx, grant, 10); err != nil {
			t.Fatalf("CreateTerminalGrant: %v", err)
		}
		if _, err := s.ConsumeTerminalGrant(ctx, grant.TokenHash); !errors.Is(err, store.ErrTerminalGrantInvalid) {
			t.Fatalf("expired consume error = %v", err)
		}
		deleted, err := s.CleanupTerminalGrants(ctx, now.Add(time.Hour))
		if err != nil {
			t.Fatalf("CleanupTerminalGrants: %v", err)
		}
		if deleted < 2 {
			t.Fatalf("deleted grants = %d, want at least 2", deleted)
		}
	})

	t.Run("session limit resume counters and idempotent close", func(t *testing.T) {
		if err := s.DB().WithContext(ctx).Exec("DELETE FROM terminal_sessions").Error; err != nil {
			t.Fatalf("reset terminal_sessions: %v", err)
		}
		activeSince := now.Add(-90 * time.Second)
		stale := terminalSession("stale", now.Add(-2*time.Minute))
		if err := s.CreateTerminalSession(ctx, stale, 2, activeSince); err != nil {
			t.Fatalf("create stale session: %v", err)
		}
		first := terminalSession("active-1", now)
		second := terminalSession("active-2", now)
		if err := s.CreateTerminalSession(ctx, first, 2, activeSince); err != nil {
			t.Fatalf("create active session 1: %v", err)
		}
		if err := s.CreateTerminalSession(ctx, second, 2, activeSince); err != nil {
			t.Fatalf("create active session 2: %v", err)
		}
		if err := s.CreateTerminalSession(ctx, terminalSession("active-3", now), 2, activeSince); !errors.Is(err, store.ErrTerminalSessionLimit) {
			t.Fatalf("third active session error = %v", err)
		}

		resumeAt := now.Add(time.Second)
		if err := s.ResumeTerminalSession(ctx, first.ID, first.UserID, first.SandboxID, first.ContainerID, resumeAt, now.Add(-30*time.Second)); err != nil {
			t.Fatalf("ResumeTerminalSession: %v", err)
		}
		if err := s.ResumeTerminalSession(ctx, first.ID, first.UserID, first.SandboxID, "other-container", resumeAt, now.Add(-30*time.Second)); !errors.Is(err, store.ErrTerminalSessionLost) {
			t.Fatalf("cross-target resume error = %v", err)
		}

		if err := s.TouchTerminalSession(ctx, first.ID, now.Add(2*time.Second), 10, 20); err != nil {
			t.Fatalf("TouchTerminalSession first: %v", err)
		}
		if err := s.TouchTerminalSession(ctx, first.ID, now.Add(3*time.Second), 5, 15); err != nil {
			t.Fatalf("TouchTerminalSession retry: %v", err)
		}
		closeAt := now.Add(4 * time.Second)
		if err := s.CloseTerminalSession(ctx, first.ID, closeAt, "RUNTIME_EXITED", nil, 12, 22); err != nil {
			t.Fatalf("CloseTerminalSession first: %v", err)
		}
		exitCode := int32(17)
		if err := s.CloseTerminalSession(ctx, first.ID, closeAt.Add(time.Second), "INTERNAL", &exitCode, 11, 30); err != nil {
			t.Fatalf("CloseTerminalSession retry: %v", err)
		}

		got, err := s.GetTerminalSession(ctx, first.ID)
		if err != nil {
			t.Fatalf("GetTerminalSession: %v", err)
		}
		if got.ResumeCount != 1 || got.BytesIn != 12 || got.BytesOut != 30 {
			t.Fatalf("session counters = %+v", got)
		}
		if !got.ClosedAt.Valid || !got.ClosedAt.Time.Equal(closeAt) || got.CloseReason.String != "RUNTIME_EXITED" || got.ExitCode.Int64 != 17 {
			t.Fatalf("idempotent close state = %+v", got)
		}
	})

	t.Run("orphan reconciliation", func(t *testing.T) {
		orphan := terminalSession("orphan", now.Add(-2*time.Minute))
		if err := s.CreateTerminalSession(ctx, orphan, 2, now.Add(-90*time.Second)); err != nil {
			t.Fatalf("create orphan: %v", err)
		}
		closed, err := s.ReconcileOrphanTerminalSessions(ctx, now.Add(-90*time.Second), now)
		if err != nil {
			t.Fatalf("ReconcileOrphanTerminalSessions: %v", err)
		}
		if closed < 1 {
			t.Fatalf("reconciled sessions = %d, want at least 1", closed)
		}
		got, err := s.GetTerminalSession(ctx, orphan.ID)
		if err != nil {
			t.Fatalf("GetTerminalSession orphan: %v", err)
		}
		if !got.ClosedAt.Valid || got.CloseReason.String != "ORPHANED" {
			t.Fatalf("orphan state = %+v", got)
		}
	})
}

func terminalGrant(label string, createdAt, expiresAt time.Time) *store.TerminalGrant {
	digest := sha256.Sum256([]byte(label))
	return &store.TerminalGrant{
		ID:           uuid.NewString(),
		TokenHash:    hex.EncodeToString(digest[:]),
		Kind:         "open",
		UserID:       "admin",
		SandboxID:    "sandbox-a",
		ContainerID:  "container-a",
		SessionID:    uuid.NewString(),
		Cols:         80,
		Rows:         24,
		ResumeOffset: 0,
		CreatedAt:    createdAt,
		ExpiresAt:    expiresAt,
	}
}

func terminalSession(label string, seenAt time.Time) *store.TerminalSession {
	return &store.TerminalSession{
		ID:          uuid.NewString(),
		UserID:      "admin",
		SandboxID:   "sandbox-" + label,
		ContainerID: "container-" + label,
		CubeletHost: "node-a",
		OpenedAt:    seenAt,
		LastSeenAt:  seenAt,
	}
}
