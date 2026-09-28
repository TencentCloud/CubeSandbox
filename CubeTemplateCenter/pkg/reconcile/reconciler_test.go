// SPDX-License-Identifier: Apache-2.0
//

package reconcile

import (
	"context"
	"strings"
	"testing"
	"time"
)

// ---- durationOverride ----

func TestDurationOverride(t *testing.T) {
	fallback := 10 * time.Minute
	cases := []struct {
		name  string
		raw   string
		found bool
		want  time.Duration
	}{
		{"not found returns fallback", "", false, fallback},
		{"valid duration", "5m", true, 5 * time.Minute},
		{"valid hours", "2h", true, 2 * time.Hour},
		{"whitespace trimmed", "  30s  ", true, 30 * time.Second},
		{"invalid returns fallback", "abc", true, fallback},
		{"zero returns fallback", "0", true, fallback},
		{"negative returns fallback", "-5m", true, fallback},
		{"empty string found returns fallback", "", true, fallback},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := durationOverride(tc.raw, tc.found, fallback)
			if got != tc.want {
				t.Fatalf("durationOverride(%q, %v, %v) = %v, want %v", tc.raw, tc.found, fallback, got, tc.want)
			}
		})
	}
}

// ---- staleThresholdFor ----

func newTestReconciler() *Reconciler {
	return &Reconciler{
		pendingStaleAfter: 10 * time.Minute,
		pullingStaleAfter: 30 * time.Minute,
		runningStaleAfter: 1 * time.Hour,
	}
}

func TestStaleThresholdFor(t *testing.T) {
	r := newTestReconciler()
	cases := []struct {
		name   string
		status string
		phase  string
		want   time.Duration
	}{
		{"pending", "PENDING", "", 10 * time.Minute},
		{"pending ignores phase", "PENDING", "PULLING", 10 * time.Minute},
		{"running pulling", "RUNNING", "PULLING", 30 * time.Minute},
		{"running building_ext4", "RUNNING", "BUILDING_EXT4", 1 * time.Hour},
		{"running distributing", "RUNNING", "DISTRIBUTING", 1 * time.Hour},
		{"running empty phase", "RUNNING", "", 1 * time.Hour},
		{"built not swept", "BUILT", "", 0},
		{"ready not swept", "READY", "", 0},
		{"failed not swept", "FAILED", "", 0},
		{"lowercase status not matched", "pending", "", 0},
		{"unknown status", "UNKNOWN", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := r.staleThresholdFor(tc.status, tc.phase)
			if got != tc.want {
				t.Fatalf("staleThresholdFor(%q, %q) = %v, want %v", tc.status, tc.phase, got, tc.want)
			}
		})
	}
}

// ---- staleMessage ----

func TestStaleMessage(t *testing.T) {
	r := newTestReconciler()
	updatedAt := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)

	t.Run("pending message mentions never picked up", func(t *testing.T) {
		job := staleJob{Status: "PENDING", Phase: "", UpdatedAt: updatedAt}
		msg := r.staleMessage(job, 10*time.Minute)
		if !strings.Contains(msg, "never picked up") {
			t.Fatalf("expected 'never picked up' in message, got: %s", msg)
		}
		if !strings.Contains(msg, "forwarding") {
			t.Fatalf("expected 'forwarding' in message, got: %s", msg)
		}
	})

	t.Run("pulling message mentions image pull stalled", func(t *testing.T) {
		job := staleJob{Status: "RUNNING", Phase: "PULLING", UpdatedAt: updatedAt}
		msg := r.staleMessage(job, 30*time.Minute)
		if !strings.Contains(msg, "image pull stalled") {
			t.Fatalf("expected 'image pull stalled' in message, got: %s", msg)
		}
		if !strings.Contains(msg, "registry") {
			t.Fatalf("expected 'registry' in message, got: %s", msg)
		}
	})

	t.Run("running other phase mentions phase name", func(t *testing.T) {
		job := staleJob{Status: "RUNNING", Phase: "BUILDING_EXT4", UpdatedAt: updatedAt}
		msg := r.staleMessage(job, 1*time.Hour)
		if !strings.Contains(msg, "BUILDING_EXT4") {
			t.Fatalf("expected phase name in message, got: %s", msg)
		}
		if !strings.Contains(msg, "interrupted") {
			t.Fatalf("expected 'interrupted' in message, got: %s", msg)
		}
	})

	t.Run("unexpected status uses default branch", func(t *testing.T) {
		job := staleJob{Status: "BUILT", Phase: "X", UpdatedAt: updatedAt}
		msg := r.staleMessage(job, time.Minute)
		if !strings.Contains(msg, "unexpected state") {
			t.Fatalf("expected 'unexpected state' in message, got: %s", msg)
		}
	})

	t.Run("message includes RFC3339 timestamp", func(t *testing.T) {
		job := staleJob{Status: "PENDING", Phase: "", UpdatedAt: updatedAt}
		msg := r.staleMessage(job, 10*time.Minute)
		if !strings.Contains(msg, updatedAt.Format(time.RFC3339)) {
			t.Fatalf("expected timestamp %s in message, got: %s", updatedAt.Format(time.RFC3339), msg)
		}
	})
}

// ---- SetDeleter ----

func TestSetDeleter(t *testing.T) {
	r := newTestReconciler()
	if r.deleter != nil {
		t.Fatalf("expected nil deleter initially")
	}
	d := &fakeDeleter{}
	r.SetDeleter(d)
	if r.deleter != d {
		t.Fatalf("expected deleter set")
	}
	// Nil deleter disables sweep.
	r.SetDeleter(nil)
	if r.deleter != nil {
		t.Fatalf("expected deleter cleared")
	}
}

type fakeDeleter struct {
	deleted []string
	err     error
}

func (f *fakeDeleter) Delete(_ context.Context, artifactID string) error {
	f.deleted = append(f.deleted, artifactID)
	return f.err
}

// cleanupPendingArtifacts with a nil deleter must be a no-op (does not touch
// the DB), which is the only branch testable without a database handle.
func TestCleanupPendingArtifactsNilDeleter(t *testing.T) {
	r := newTestReconciler()
	r.deleter = nil
	// db is nil; a nil-deleter sweep must return nil without dereferencing it.
	if err := r.cleanupPendingArtifacts(context.Background()); err != nil {
		t.Fatalf("expected nil for nil deleter, got %v", err)
	}
}
