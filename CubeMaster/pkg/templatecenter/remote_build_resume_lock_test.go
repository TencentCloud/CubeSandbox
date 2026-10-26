// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"sync"
	"testing"
	"time"
)

// Regression test for the P2 unbounded-map-growth bug: resumeArtifactLocks
// used to keep one entry PER artifact_id EVER PROCESSED, for the lifetime of
// the process. Once a resume completes and nothing else references the
// artifact_id, the entry must be removed.
func TestAcquireResumeArtifactLockRemovesEntryAfterRelease(t *testing.T) {
	resumeArtifactLocksMu.Lock()
	resumeArtifactLocks = map[string]*refCountedMutex{}
	resumeArtifactLocksMu.Unlock()

	release := acquireResumeArtifactLock("artifact-1")
	resumeArtifactLocksMu.Lock()
	if _, ok := resumeArtifactLocks["artifact-1"]; !ok {
		resumeArtifactLocksMu.Unlock()
		t.Fatal("expected entry to exist while lock is held")
	}
	resumeArtifactLocksMu.Unlock()

	release()

	resumeArtifactLocksMu.Lock()
	defer resumeArtifactLocksMu.Unlock()
	if _, ok := resumeArtifactLocks["artifact-1"]; ok {
		t.Fatal("expected entry to be removed after release, map is leaking")
	}
}

// Two concurrent callers for the SAME artifact_id (e.g. two different
// job_ids that resolved to the same artifact via dedup/reuse, or a callback
// racing a reconciler replay) must serialize (the original mutual-exclusion
// contract), and the entry must only be removed once BOTH have released --
// releasing while a second caller still waits must not delete the entry out
// from under it.
func TestAcquireResumeArtifactLockSerializesSameArtifactID(t *testing.T) {
	resumeArtifactLocksMu.Lock()
	resumeArtifactLocks = map[string]*refCountedMutex{}
	resumeArtifactLocksMu.Unlock()

	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup

	release1 := acquireResumeArtifactLock("artifact-2")

	wg.Add(1)
	go func() {
		defer wg.Done()
		release2 := acquireResumeArtifactLock("artifact-2") // blocks until release1() runs
		mu.Lock()
		order = append(order, 2)
		mu.Unlock()
		release2()
	}()

	time.Sleep(20 * time.Millisecond) // let the goroutine block on the lock
	mu.Lock()
	order = append(order, 1)
	mu.Unlock()
	release1()

	wg.Wait()

	if len(order) != 2 || order[0] != 1 || order[1] != 2 {
		t.Fatalf("order = %v, want [1 2] (second acquirer must wait for the first release)", order)
	}

	resumeArtifactLocksMu.Lock()
	defer resumeArtifactLocksMu.Unlock()
	if _, ok := resumeArtifactLocks["artifact-2"]; ok {
		t.Fatal("expected entry to be removed once both callers released")
	}
}

// Different artifact_ids must not block each other.
func TestAcquireResumeArtifactLockDoesNotSerializeDifferentArtifactIDs(t *testing.T) {
	resumeArtifactLocksMu.Lock()
	resumeArtifactLocks = map[string]*refCountedMutex{}
	resumeArtifactLocksMu.Unlock()

	release1 := acquireResumeArtifactLock("artifact-a")
	done := make(chan struct{})
	go func() {
		release2 := acquireResumeArtifactLock("artifact-b")
		release2()
		close(done)
	}()

	select {
	case <-done:
		// good: artifact-b did not wait on artifact-a's lock
	case <-time.After(2 * time.Second):
		t.Fatal("acquireResumeArtifactLock for a different artifact_id blocked; locks are not per-artifact")
	}
	release1()
}
