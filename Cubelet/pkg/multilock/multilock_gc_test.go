// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package multilock

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetKeepsAnActivelyUsedLockAlive(t *testing.T) {
	m := NewMultiLock(&Options{CheckInterval: 20 * time.Millisecond, ExpiredInSecond: 2})

	first := m.Get("sha256:volume-A")
	first.Lock()
	first.Unlock()

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		again := m.Get("sha256:volume-A")
		if again != first {
			t.Fatal("Get returned a different lock object for a key that is being used")
		}
		again.Lock()
		again.Unlock()
		time.Sleep(30 * time.Millisecond)
	}

	if _, mapped := m.Load("sha256:volume-A"); !mapped {
		t.Fatal("an actively used lock was evicted from the map")
	}
}

func TestMutualExclusionHoldsAcrossGCTicks(t *testing.T) {
	m := NewMultiLock(&Options{CheckInterval: 20 * time.Millisecond, ExpiredInSecond: 2})

	var inCritical, maxConcurrent int32
	var wg sync.WaitGroup

	worker := func() {
		defer wg.Done()
		for i := 0; i < 40; i++ {
			l := m.Get("sha256:volume-A")
			l.Lock()
			n := atomic.AddInt32(&inCritical, 1)
			for {
				old := atomic.LoadInt32(&maxConcurrent)
				if n <= old || atomic.CompareAndSwapInt32(&maxConcurrent, old, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			atomic.AddInt32(&inCritical, -1)
			l.Unlock()
		}
	}

	wg.Add(4)
	for i := 0; i < 4; i++ {
		go worker()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&maxConcurrent); got != 1 {
		t.Fatalf("max concurrent holders of one key = %d, want 1", got)
	}
}

func TestIdleLockIsStillEvicted(t *testing.T) {
	m := NewMultiLock(&Options{CheckInterval: 20 * time.Millisecond, ExpiredInSecond: 0})

	m.Get("sha256:volume-idle")
	time.Sleep(1500 * time.Millisecond)

	if _, mapped := m.Load("sha256:volume-idle"); mapped {
		t.Fatal("an idle lock was not evicted; the GC no longer reclaims anything")
	}
}
