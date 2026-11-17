// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package filter

import (
	"context"
	"sync"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

func newTestSelCtx() *selctx.SelectorCtx {
	s := selctx.New("random")
	s.Ctx = context.Background()
	return s
}

// The reject-line budget is per request, not per filter: several filters
// inspect the same candidate set, so a per-filter budget multiplied log volume
// by the filter count.
func TestRejectLogBudgetSharedAcrossFilters(t *testing.T) {
	selCtx := newTestSelCtx()

	var shown int
	var mu sync.Mutex
	// Four filters, each rejecting 10 nodes, as a clone burst does.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				if selCtx.TakeRejectLogSlot() {
					mu.Lock()
					shown++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()

	// 40 rejections across four filters must not print 40 lines; the shared
	// per-request budget bounds it.
	if shown != 3 {
		t.Fatalf("printed %d reject lines for one request, want the shared budget 3", shown)
	}
}

func TestRejectLogBudgetsAreIndependentPerRequest(t *testing.T) {
	first, second := newTestSelCtx(), newTestSelCtx()
	for i := 0; i < 3; i++ {
		if !first.TakeRejectLogSlot() {
			t.Fatalf("slot %d must be granted for the first request", i)
		}
	}
	if first.TakeRejectLogSlot() {
		t.Fatal("first request exceeded its budget")
	}
	if !second.TakeRejectLogSlot() {
		t.Fatal("a new request must start with a fresh budget")
	}
}

func TestRejectLogCountsSuppressedRejects(t *testing.T) {
	selCtx := newTestSelCtx()
	rejects := newRejectLog(selCtx)

	for i := 0; i < 10; i++ {
		rejects.warnf("%v select:%v rejected", "filter/test", i)
	}
	// The budget bounds what is printed; everything beyond it is counted so
	// flush can summarise it.
	if rejects.hidden != 7 {
		t.Fatalf("hidden = %d, want 7", rejects.hidden)
	}
}

func TestRejectLogFlushOnlyOnSample(t *testing.T) {
	// The summary itself must not become a per-request line.
	rejects := newRejectLog(newTestSelCtx())
	rejects.hidden = 5

	base := rejectSampler.Load()
	defer rejectSampler.Store(base)

	// Nothing is due until the sampler reaches the next sampling boundary.
	rejectSampler.Store(base + rejectSampleRate - (base % rejectSampleRate) - 1)
	if rejectSampler.Add(1)%rejectSampleRate != 0 {
		t.Fatalf("sampler did not land on a boundary; test setup is wrong")
	}
}

func TestRejectLogNilSafety(t *testing.T) {
	var r *rejectLog
	r.warnf("noop")  // must not panic
	r.errorf("noop") // must not panic
	r.flush("filter/test")
}
