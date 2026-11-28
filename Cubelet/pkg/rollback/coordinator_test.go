// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package rollback

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func mustCoordinator(t *testing.T, dir string, opts Options) *Coordinator {
	t.Helper()
	if opts.Execute == nil {
		opts.Execute = func(context.Context, *Task) error { return nil }
	}
	c, err := New(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func mustPrepare(t *testing.T, c *Coordinator, id string) *Task {
	t.Helper()
	task, err := c.Prepare(id, "default", "request")
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestJournalRecoversCreateIntentAndRuntimeEvidence(t *testing.T) {
	dir := t.TempDir()
	c := mustCoordinator(t, dir, Options{})
	task := mustPrepare(t, c, "sandbox")
	intent := time.Time{}
	e := Evidence{
		Identities:  []Identity{{PID: 42, StartTime: 123}},
		Unresolved:  []Candidate{{PID: 43, Source: "bundle", Trusted: true, Identity: &Identity{PID: 43, StartTime: 456}}},
		SpawnIntent: &intent,
	}
	if _, err := task.Remember(e); err != nil {
		t.Fatal(err)
	}
	// No Activate: emulate a crash after create started, before failover.
	recovered := mustCoordinator(t, dir, Options{})
	loaded := recovered.Task("sandbox")
	if loaded == nil {
		t.Fatal("durable gate was lost")
	}
	r := loaded.Snapshot()
	if r.Preparing || !reflect.DeepEqual(e, r.Evidence) {
		t.Fatalf("recovered record = %+v", r)
	}
	// Neither the caller nor a snapshot can mutate the stored process identity.
	r.Evidence.Unresolved[0].Identity.StartTime++
	if loaded.Snapshot().Evidence.Unresolved[0].Identity.StartTime != 456 {
		t.Fatal("snapshot aliases task state")
	}
	if _, err := loaded.Remember(Evidence{}); err != nil {
		t.Fatal(err)
	}
	if len(loaded.Snapshot().Evidence.Identities) != 1 {
		t.Fatal("verified identity lost on retry")
	}
	if err := recovered.Join(t.Context(), "sandbox"); err != nil {
		t.Fatal(err)
	}
	if mustCoordinator(t, dir, Options{}).Pending("sandbox") {
		t.Fatal("successful cleanup left a durable gate")
	}
}

func TestJournalFailurePreventsAdmissionAndEvidenceLoss(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	c := mustCoordinator(t, dir, Options{})
	task := mustPrepare(t, c, "sandbox")
	// Replace the directory, simulating an unavailable journal filesystem.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	e := Evidence{Identities: []Identity{{PID: 42, StartTime: 123}}}
	if _, err := task.Remember(e); err == nil {
		t.Fatal("teardown must fail if evidence cannot be persisted")
	}
	if !c.Pending("sandbox") || len(task.Snapshot().Evidence.Identities) != 1 {
		t.Fatal("in-memory safety evidence was discarded")
	}
	if _, err := c.Prepare("new", "default", "request"); err == nil {
		t.Fatal("must not allocate resources without a durable intent")
	}
	if c.Pending("new") {
		t.Fatal("failed admission published a task")
	}
}

func TestTransientSchedulingDoesNotQuarantineAndDeleteBypassesDelay(t *testing.T) {
	var mode atomic.Int32
	var calls atomic.Int32
	c := mustCoordinator(t, t.TempDir(), Options{
		FastAttempts: 2, SlowInterval: time.Hour,
		Execute: func(context.Context, *Task) error {
			calls.Add(1)
			switch mode.Load() {
			case 0:
				return Defer(errors.New("limiter saturated / lifecycle lock busy"))
			case 1:
				return errors.New("runtime still alive")
			default:
				return nil
			}
		},
	})
	task := mustPrepare(t, c, "sandbox")
	for range 65 {
		if c.Join(t.Context(), "sandbox") == nil {
			t.Fatal("expected deferred cleanup")
		}
	}
	if r := task.Snapshot(); r.Failures != 0 || !r.NextRetry.IsZero() {
		t.Fatalf("scheduling failures spent cleanup budget: %+v", r)
	}
	mode.Store(1)
	for range 2 {
		if c.Join(t.Context(), "sandbox") == nil {
			t.Fatal("expected cleanup failure")
		}
	}
	r := task.Snapshot()
	if r.Failures != 2 || !r.NextRetry.After(time.Now()) {
		t.Fatalf("cleanup failure did not escalate: %+v", r)
	}
	before := calls.Load()
	c.Tick()
	if calls.Load() != before {
		t.Fatal("normal driver ignored quarantine delay")
	}
	mode.Store(2)
	if err := c.Join(t.Context(), "sandbox"); err != nil {
		t.Fatal(err)
	}
	if c.Pending("sandbox") {
		t.Fatal("explicit DELETE did not recover quarantined task")
	}
}

func TestConcurrentDeleteJoinsOneCleanupAndCancellationDoesNotStopIt(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	c := mustCoordinator(t, t.TempDir(), Options{Execute: func(ctx context.Context, _ *Task) error {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}})
	mustPrepare(t, c, "sandbox")
	if err := c.Activate("sandbox"); err != nil {
		t.Fatal(err)
	}
	<-started
	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	if err := c.Join(ctx, "sandbox"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("join = %v", err)
	}
	if !c.Pending("sandbox") {
		t.Fatal("caller cancellation discarded shared cleanup")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- c.Join(t.Context(), "sandbox") }()
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("cleanup executed %d times", calls.Load())
	}
}

func TestRetryDriverSurvivesPanickingIteration(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	panics := make(chan error, 1)
	executed := make(chan struct{}, 1)
	c := mustCoordinator(t, t.TempDir(), Options{
		Context: ctx, Interval: time.Millisecond,
		OnDriverError: func(err error) {
			select {
			case panics <- err:
			default:
			}
		},
		Execute: func(context.Context, *Task) error { executed <- struct{}{}; return nil },
	})
	// Inject a faulty iteration, without involving the unrelated DeadGC loop.
	c.tasks["broken iteration"] = nil
	go c.Run()
	select {
	case <-panics:
	case <-time.After(time.Second):
		t.Fatal("driver did not recover")
	}
	c.mu.Lock()
	delete(c.tasks, "broken iteration")
	c.mu.Unlock()
	task := mustPrepare(t, c, "sandbox")
	task.mu.Lock()
	task.record.Preparing = false
	task.mu.Unlock()
	select {
	case <-executed:
	case <-time.After(time.Second):
		t.Fatal("driver stopped after panic")
	}
}

func TestExecutorPanicKeepsDurableTaskAndAllowsRetry(t *testing.T) {
	var calls atomic.Int32
	c := mustCoordinator(t, t.TempDir(), Options{Execute: func(context.Context, *Task) error {
		if calls.Add(1) == 1 {
			panic("broken cleanup")
		}
		return nil
	}})
	task := mustPrepare(t, c, "sandbox")
	if err := c.Join(t.Context(), "sandbox"); err == nil {
		t.Fatal("panic treated as cleanup success")
	}
	if task.Snapshot().Failures != 1 || !c.Pending("sandbox") {
		t.Fatal("panic lost cleanup state")
	}
	if err := c.Join(t.Context(), "sandbox"); err != nil {
		t.Fatal(err)
	}
}

func TestCorruptJournalFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "corrupt.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(dir, Options{Execute: func(context.Context, *Task) error { return nil }}); err == nil {
		t.Fatal("corrupt pending state was silently dropped")
	}
}

// Simulate both unlink failure (record remains) and directory-fsync failure
// after unlink. The completion record itself still uses the real journal.
type removalFailureStore struct {
	journalStore
	removeFirst bool
}

func (s removalFailureStore) remove(id string) error {
	if s.removeFirst {
		if err := s.journalStore.remove(id); err != nil {
			return err
		}
	}
	return errors.New("injected journal removal failure")
}

func TestCompletedResumeRemovalFailureNeverReplaysCleanup(t *testing.T) {
	for _, removed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unlink", true: "directory fsync"}[removed], func(t *testing.T) {
			dir := t.TempDir()
			var executions atomic.Int32
			var warnings atomic.Int32
			opts := Options{
				Execute:       func(context.Context, *Task) error { executions.Add(1); return nil },
				OnDriverError: func(error) { warnings.Add(1) },
			}
			c := mustCoordinator(t, dir, opts)
			task := mustPrepare(t, c, "resumed")
			c.store = removalFailureStore{journalStore: c.store, removeFirst: removed}
			if err := c.Complete(task); err != nil {
				t.Fatalf("successful resume must not become a create failure: %v", err)
			}
			if c.Pending("resumed") || !task.Snapshot().Completed || warnings.Load() != 1 {
				t.Fatal("completion must clear the gate and report housekeeping failure")
			}
			if err := c.Activate("resumed"); err == nil {
				t.Fatal("completed create was still eligible for rollback")
			}
			_, records, err := openJournal(dir)
			if err != nil {
				t.Fatal(err)
			}
			if !removed && (len(records) != 1 || !records[0].Completed) {
				t.Fatalf("remaining record must durably mark completion: %+v", records)
			}
			recovered := mustCoordinator(t, dir, opts)
			recovered.Tick()
			if err := recovered.Join(t.Context(), "resumed"); err != nil {
				t.Fatal(err)
			}
			if recovered.Pending("resumed") || executions.Load() != 0 {
				t.Fatal("recovery replayed cleanup for a successful resume")
			}
		})
	}
}

func TestCompletionWriteFailureRetainsUncommittedIntent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	c := mustCoordinator(t, dir, Options{})
	task := mustPrepare(t, c, "sandbox")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Complete(task); err == nil {
		t.Fatal("completion must not be acknowledged without durable state")
	}
	if !c.Pending("sandbox") || task.Snapshot().Completed {
		t.Fatal("failed completion write discarded the uncommitted intent")
	}
}
