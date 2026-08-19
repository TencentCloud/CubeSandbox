// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package pool

import (
	"math"
	"sync"
	"sync/atomic"
	"testing"
)

func newRefProbe(ref int32) *pool {
	return &pool{ref: ref, opt: Options{MaxIdle: 1}}
}

func TestIncrRefDoesNotOverflowPastMaxInt32(t *testing.T) {
	p := newRefProbe(math.MaxInt32)

	if got := p.incrRef(); got != math.MaxInt32 {
		t.Fatalf("incrRef returned %d, want math.MaxInt32", got)
	}
	if stored := atomic.LoadInt32(&p.ref); stored != math.MaxInt32 {
		t.Fatalf("stored ref = %d, want math.MaxInt32 (it must not wrap negative)", stored)
	}
}

func TestIncrRefIncrementsNormally(t *testing.T) {
	p := newRefProbe(0)

	if got := p.incrRef(); got != 1 {
		t.Fatalf("incrRef returned %d, want 1", got)
	}
	if got := p.incrRef(); got != 2 {
		t.Fatalf("incrRef returned %d, want 2", got)
	}
	if stored := atomic.LoadInt32(&p.ref); stored != 2 {
		t.Fatalf("stored ref = %d, want 2", stored)
	}
}

func TestDecrRefDoesNotGoNegative(t *testing.T) {
	p := newRefProbe(0)

	p.decrRef()
	if stored := atomic.LoadInt32(&p.ref); stored != 0 {
		t.Fatalf("stored ref = %d after an unbalanced decrRef, want 0", stored)
	}

	p.decrRef()
	p.decrRef()
	if stored := atomic.LoadInt32(&p.ref); stored != 0 {
		t.Fatalf("stored ref = %d after repeated unbalanced decrRef, want 0", stored)
	}
}

func TestDecrRefDecrementsNormally(t *testing.T) {
	p := newRefProbe(3)

	p.decrRef()
	if stored := atomic.LoadInt32(&p.ref); stored != 2 {
		t.Fatalf("stored ref = %d, want 2", stored)
	}
}

func TestRefCountIsBalancedUnderConcurrency(t *testing.T) {
	p := newRefProbe(0)

	const workers = 64
	const iterations = 500

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				p.incrRef()
				p.decrRef()
			}
		}()
	}
	wg.Wait()

	if stored := atomic.LoadInt32(&p.ref); stored != 0 {
		t.Fatalf("stored ref = %d after balanced concurrent use, want 0", stored)
	}
}
