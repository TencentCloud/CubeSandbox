// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package pool

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
)

func TestDecrRefUnderflow(t *testing.T) {
	p := &pool{
		ref:     0,
		current: 1,
	}

	// Calling decrRef when ref is 0 should NOT drive p.ref below 0
	p.decrRef()
	if currentRef := atomic.LoadInt32(&p.ref); currentRef < 0 {
		t.Fatalf("expected ref >= 0 after underflow decrRef, got %d", currentRef)
	}

	// Multiple redundant decrRef calls should stay clamped at 0
	for i := 0; i < 5; i++ {
		p.decrRef()
	}
	if currentRef := atomic.LoadInt32(&p.ref); currentRef != 0 {
		t.Fatalf("expected ref == 0 after multiple redundant decrRefs, got %d", currentRef)
	}

	// Subsequent incrRef should correctly increment from 0 to 1
	newRef := p.incrRef()
	if newRef != 1 {
		t.Fatalf("expected incrRef to return 1, got %d", newRef)
	}
	if currentRef := atomic.LoadInt32(&p.ref); currentRef != 1 {
		t.Fatalf("expected p.ref == 1, got %d", currentRef)
	}
}

func TestGracefulStopImmediateWhenZeroRef(t *testing.T) {
	p := &pool{
		ref:     0,
		current: 1,
	}

	start := time.Now()
	p.GracefulStop(1 * time.Second)
	elapsed := time.Since(start)

	if elapsed > 100*time.Millisecond {
		t.Fatalf("GracefulStop took %v with ref=0; should have returned immediately", elapsed)
	}
}

func TestConcurrentIncrDecrRef(t *testing.T) {
	p := &pool{
		ref:     0,
		current: 1,
	}

	var wg sync.WaitGroup
	workers := 20
	iterations := 1000

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				p.incrRef()
				if cur := atomic.LoadInt32(&p.ref); cur < 0 {
					t.Errorf("ref underflow during concurrent incr: %d", cur)
				}
				p.decrRef()
				if cur := atomic.LoadInt32(&p.ref); cur < 0 {
					t.Errorf("ref underflow during concurrent decr: %d", cur)
				}
			}
		}()
	}

	wg.Wait()

	if ref := atomic.LoadInt32(&p.ref); ref != 0 {
		t.Fatalf("expected ref == 0 after balanced concurrent ops, got: %d", ref)
	}
}

func TestGracefulStopTimeout(t *testing.T) {
	p := &pool{
		ref:     2,
		current: 1,
	}

	start := time.Now()
	p.GracefulStop(50 * time.Millisecond)
	elapsed := time.Since(start)

	if elapsed < 40*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("GracefulStop waited unexpected duration: %v", elapsed)
	}
}

func TestIncrRefOverflowProtection(t *testing.T) {
	p := &pool{
		ref:     2147483646,
		current: 1,
	}

	r1 := p.incrRef()
	if r1 != 2147483647 {
		t.Fatalf("expected 2147483647, got %d", r1)
	}
	r2 := p.incrRef()
	if r2 != 2147483647 {
		t.Fatalf("expected clamped at MaxInt32 (2147483647), got %d", r2)
	}
	if currentRef := atomic.LoadInt32(&p.ref); currentRef != 2147483647 {
		t.Fatalf("expected p.ref to be 2147483647, got %d", currentRef)
	}
}

func TestGetAfterCloseNoRefLeak(t *testing.T) {
	p := &pool{
		ref:     0,
		current: 0, // closed pool
	}

	_, err := p.Get()
	if err != ErrClosed {
		t.Fatalf("expected ErrClosed, got %v", err)
	}

	if ref := atomic.LoadInt32(&p.ref); ref != 0 {
		t.Fatalf("expected ref == 0 after Get() on closed pool, got %d", ref)
	}
}

func TestDecrRefShrinkPath(t *testing.T) {
	p := &pool{
		ref:     1,
		current: 4,
		opt: Options{
			MaxIdle:   2,
			MaxActive: 4,
		},
		conns: make([]*conn, 4),
	}

	for i := range p.conns {
		p.conns[i] = &conn{pool: p}
	}

	p.decrRef()

	if ref := atomic.LoadInt32(&p.ref); ref != 0 {
		t.Fatalf("expected ref == 0 after decrRef, got %d", ref)
	}
	if current := atomic.LoadInt32(&p.current); current != 2 {
		t.Fatalf("expected current reset to MaxIdle (2), got %d", current)
	}
	for i := 2; i < 4; i++ {
		if p.conns[i] != nil {
			t.Fatalf("expected conn at index %d to be nil after shrink, got %v", i, p.conns[i])
		}
	}
}

func TestGetDialFailureNoRefLeak(t *testing.T) {
	dialErr := errors.New("dial failure")
	p := &pool{
		ref:     2,
		current: 2,
		opt: Options{
			MaxIdle:              1,
			MaxActive:            2,
			MaxConcurrentStreams: 1,
			Reuse:                false,
			Dial: func(ua, address string) (*grpc.ClientConn, error) {
				return nil, dialErr
			},
		},
		conns: make([]*conn, 2),
	}

	// Case 1: current >= MaxActive with Reuse=false dial error
	_, err := p.Get()
	if err != dialErr {
		t.Fatalf("expected dialErr, got %v", err)
	}
	if ref := atomic.LoadInt32(&p.ref); ref != 2 {
		t.Fatalf("expected ref == 2 after dial failure in Get(), got %d", ref)
	}

	// Case 2: expansion block dial error
	p2 := &pool{
		ref:     1,
		current: 1,
		opt: Options{
			MaxIdle:              1,
			MaxActive:            4,
			MaxConcurrentStreams: 1,
			Dial: func(ua, address string) (*grpc.ClientConn, error) {
				return nil, dialErr
			},
		},
		conns: make([]*conn, 4),
	}
	_, err2 := p2.Get()
	if err2 != dialErr {
		t.Fatalf("expected dialErr, got %v", err2)
	}
	if ref := atomic.LoadInt32(&p2.ref); ref != 1 {
		t.Fatalf("expected ref == 1 after expansion dial failure in Get(), got %d", ref)
	}
}

func TestGetNilConnAfterCloseRace(t *testing.T) {
	p := &pool{
		ref:     0,
		current: 1,
		opt: Options{
			OptionType: SingleConn,
		},
		conns: make([]*conn, 1),
	}
	_, err := p.Get()
	if err != ErrClosed {
		t.Fatalf("expected ErrClosed when conns[0] is nil, got %v", err)
	}
	if ref := atomic.LoadInt32(&p.ref); ref != 0 {
		t.Fatalf("expected ref == 0 after nil conn check, got %d", ref)
	}
}
