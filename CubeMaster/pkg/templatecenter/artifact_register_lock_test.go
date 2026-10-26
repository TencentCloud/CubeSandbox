// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPollSessionLockRetriesUntilAcquired(t *testing.T) {
	attempts := 0
	locked, err := pollSessionLock(context.Background(), time.Second, time.Millisecond, func() (bool, error) {
		attempts++
		return attempts == 3, nil
	})
	if err != nil {
		t.Fatalf("pollSessionLock() error = %v", err)
	}
	if !locked {
		t.Fatal("pollSessionLock() did not acquire the lock")
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestPollSessionLockTimesOutWithoutBlockingSQL(t *testing.T) {
	attempts := 0
	locked, err := pollSessionLock(context.Background(), 15*time.Millisecond, time.Millisecond, func() (bool, error) {
		attempts++
		return false, nil
	})
	if err != nil {
		t.Fatalf("pollSessionLock() error = %v", err)
	}
	if locked {
		t.Fatal("pollSessionLock() acquired a lock that stayed busy")
	}
	if attempts < 2 {
		t.Fatalf("attempts = %d, want multiple non-blocking attempts", attempts)
	}
}

func TestPollSessionLockStopsOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	locked, err := pollSessionLock(ctx, time.Second, time.Millisecond, func() (bool, error) {
		attempts++
		if attempts == 2 {
			cancel()
		}
		return false, nil
	})
	if locked {
		t.Fatal("pollSessionLock() acquired a lock after cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pollSessionLock() error = %v, want context.Canceled", err)
	}
}

func TestPollSessionLockReturnsAttemptError(t *testing.T) {
	want := errors.New("invalid connection")
	locked, err := pollSessionLock(context.Background(), time.Second, time.Millisecond, func() (bool, error) {
		return false, want
	})
	if locked {
		t.Fatal("pollSessionLock() acquired a lock after an attempt error")
	}
	if !errors.Is(err, want) {
		t.Fatalf("pollSessionLock() error = %v, want %v", err, want)
	}
}
