// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package pool

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

const testTarget = "passthrough:///localhost:1"

func testDialer(t testing.TB) func() (*grpc.ClientConn, error) {
	var mu sync.Mutex
	var ccs []*grpc.ClientConn
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, cc := range ccs {
			_ = cc.Close()
		}
	})
	return func() (*grpc.ClientConn, error) {
		cc, err := grpc.Dial(testTarget, grpc.WithInsecure())
		if err != nil {
			return nil, err
		}
		mu.Lock()
		ccs = append(ccs, cc)
		mu.Unlock()
		return cc, nil
	}
}

// newTestPool builds a valid pool with `current` live slots. Slots above
// current stay nil, mirroring a freshly created pool.
func newTestPool(t testing.TB, opt Options, current int) *pool {
	t.Helper()
	if opt.MaxIdle <= 0 {
		opt.MaxIdle = 1
	}
	if opt.MaxActive <= 0 {
		opt.MaxActive = opt.MaxIdle
	}
	if opt.MaxIdle > opt.MaxActive {
		opt.MaxIdle = opt.MaxActive
	}
	if opt.OptionType != SingleConn && opt.MaxConcurrentStreams <= 0 {
		opt.MaxConcurrentStreams = 1
	}

	slots := opt.MaxActive
	if opt.OptionType == SingleConn {
		slots = 1
		current = 1
	}
	if current <= 0 {
		current = opt.MaxIdle
	}
	if current > slots {
		current = slots
	}

	p := newPool("test-address", opt, testDialer(t))
	p.conns = make([]*poolConn, slots)
	p.current = int32(current)
	p.shrinkable.Store(int32(current) > int32(opt.MaxIdle))
	for i := 0; i < current; i++ {
		cc, err := p.dial()
		if err != nil {
			t.Fatalf("dial slot %d: %v", i, err)
		}
		p.conns[i] = &poolConn{cc: cc}
	}
	return p
}

func singleConnOptions() Options {
	return Options{
		OptionType: SingleConn,
		MaxIdle:    1,
		MaxActive:  1,
		Reuse:      true,
	}
}

func TestLeaseValueStableAfterPoolClose(t *testing.T) {
	p := newTestPool(t, singleConnOptions(), 1)

	c, err := p.Get()
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	cc := c.Value()
	if cc == nil {
		t.Fatal("Value is nil before Close")
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if ref := p.ref.Load(); ref != 1 {
		t.Fatalf("Close must not zero outstanding references, got ref=%d", ref)
	}
	if c.Value() == nil {
		t.Fatal("Value became nil after pool.Close")
	}
	if cc.GetState() != connectivity.Shutdown {
		t.Fatalf("pool.Close did not shut down the captured connection: %v", cc.GetState())
	}
	if err := c.Close(); err != nil {
		t.Fatalf("lease.Close: %v", err)
	}
	if ref := p.ref.Load(); ref != 0 {
		t.Fatalf("expected ref 0 after lease release, got %d", ref)
	}
}

func TestConcurrentGetCloseNeverNil(t *testing.T) {
	for round := 0; round < 20; round++ {
		p := newTestPool(t, singleConnOptions(), 1)
		start := make(chan struct{})
		var wg sync.WaitGroup

		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for j := 0; j < 100; j++ {
					c, err := p.Get()
					if err != nil {
						continue
					}
					if c.Value() == nil {
						t.Errorf("Get returned nil connection with nil error")
						return
					}
					_ = c.Close()
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_ = p.Close()
		}()

		close(start)
		wg.Wait()
		_ = p.Close()
	}
}

func TestLeaseCloseIdempotent(t *testing.T) {
	p := newTestPool(t, Options{
		OptionType:           ConnPool,
		MaxIdle:              2,
		MaxActive:            4,
		MaxConcurrentStreams: 1,
		Reuse:                true,
	}, 4)

	a, err := p.Get()
	if err != nil {
		t.Fatalf("Get a: %v", err)
	}
	b, err := p.Get()
	if err != nil {
		t.Fatalf("Get b: %v", err)
	}
	if ref := p.ref.Load(); ref != 2 {
		t.Fatalf("expected ref 2, got %d", ref)
	}

	_ = a.Close()
	_ = a.Close()
	if ref := p.ref.Load(); ref != 1 {
		t.Fatalf("duplicate lease.Close released more than once: ref=%d", ref)
	}
	if p.current != 4 {
		t.Fatalf("duplicate lease.Close must not shrink, current=%d", p.current)
	}
	for i, c := range p.conns {
		if c == nil {
			t.Fatalf("duplicate lease.Close detached conns[%d]", i)
		}
	}
	if b.Value() == nil {
		t.Fatal("outstanding lease lost its connection")
	}

	_ = b.Close()
	if ref := p.ref.Load(); ref != 0 {
		t.Fatalf("expected ref 0, got %d", ref)
	}
	if p.current != 2 {
		t.Fatalf("expected shrink to MaxIdle after last release, got current=%d", p.current)
	}
}

func TestExpansionThenShrink(t *testing.T) {
	p := newTestPool(t, Options{
		OptionType:           ConnPool,
		MaxIdle:              1,
		MaxActive:            4,
		MaxConcurrentStreams: 1,
		Reuse:                true,
	}, 1)
	defer p.Close()

	first, err := p.Get()
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	if p.current != 1 {
		t.Fatalf("expected current 1 before expansion, got %d", p.current)
	}

	second, err := p.Get()
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if p.current <= 1 {
		t.Fatalf("expected expansion above MaxIdle, current=%d", p.current)
	}
	if !p.shrinkable.Load() {
		t.Fatal("expected shrinkable after expansion")
	}

	p.mu.RLock()
	expanded := p.conns[1]
	p.mu.RUnlock()
	if expanded == nil || expanded.cc == nil {
		t.Fatal("expanded slot is not populated")
	}
	expandedCC := expanded.cc

	_ = second.Close()
	if p.current <= 1 {
		t.Fatalf("pool shrank while a lease was still outstanding: current=%d", p.current)
	}

	_ = first.Close()
	if p.current != 1 {
		t.Fatalf("expected shrink to MaxIdle after last release, got current=%d", p.current)
	}
	if p.shrinkable.Load() {
		t.Fatal("expected shrinkable to be cleared after shrink")
	}
	p.mu.RLock()
	expanded = p.conns[1]
	p.mu.RUnlock()
	if expanded != nil {
		t.Fatalf("expected expanded slot detached, got %+v", expanded)
	}
	if expandedCC.GetState() != connectivity.Shutdown {
		t.Fatalf("expanded connection was not closed: %v", expandedCC.GetState())
	}
}

func TestPoolCloseWaitsForTeardown(t *testing.T) {
	p := newTestPool(t, singleConnOptions(), 1)
	cc := p.conns[0].cc

	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	p.closeFn = func(conns []*poolConn) error {
		once.Do(func() { close(started) })
		<-release
		return closePoolConns(conns)
	}

	done1 := make(chan error, 1)
	go func() { done1 <- p.Close() }()
	<-started

	done2 := make(chan error, 1)
	go func() { done2 <- p.Close() }()
	time.Sleep(50 * time.Millisecond)
	select {
	case <-done2:
		t.Fatal("second Close returned before teardown completed")
	default:
	}

	close(release)
	if err := <-done1; err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := <-done2; err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if cc.GetState() != connectivity.Shutdown {
		t.Fatalf("Close returned before the bound connection was shut down: %v", cc.GetState())
	}
}

func TestPoolCloseIdempotentConcurrent(t *testing.T) {
	p := newTestPool(t, singleConnOptions(), 1)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = p.Close()
		}()
	}
	wg.Wait()

	if !p.closed {
		t.Fatal("expected pool to be closed")
	}
	if p.current != 0 {
		t.Fatalf("expected current 0 after Close, got %d", p.current)
	}
	for i, c := range p.conns {
		if c != nil {
			t.Fatalf("expected conns[%d] nil after Close", i)
		}
	}
}

func TestConcurrentGetCloseBalancesRef(t *testing.T) {
	p := newTestPool(t, singleConnOptions(), 1)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				c, err := p.Get()
				if err != nil {
					t.Errorf("Get: %v", err)
					return
				}
				if c.Value() == nil {
					t.Errorf("nil connection")
					return
				}
				_ = c.Close()
			}
		}()
	}
	wg.Wait()

	if ref := p.ref.Load(); ref != 0 {
		t.Fatalf("expected ref 0 after balanced Get/Close, got %d", ref)
	}
}

func TestGetAfterCloseNoRefLeak(t *testing.T) {
	p := newTestPool(t, singleConnOptions(), 1)
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, err := p.Get()
	if err != ErrClosed {
		t.Fatalf("expected ErrClosed, got %v", err)
	}
	if ref := p.ref.Load(); ref != 0 {
		t.Fatalf("expected ref 0 after Get on closed pool, got %d", ref)
	}
}

func TestNewPoolWithSlotsConnPool(t *testing.T) {
	opt := Options{
		OptionType:           ConnPool,
		MaxIdle:              2,
		MaxActive:            4,
		MaxConcurrentStreams: 2,
		Reuse:                true,
	}
	p, err := newPoolWithSlots("test-address", opt, testDialer(t))
	if err != nil {
		t.Fatalf("newPoolWithSlots: %v", err)
	}
	if p.current != 2 {
		t.Fatalf("current = %d, want 2", p.current)
	}
	if len(p.conns) != 4 {
		t.Fatalf("len(conns) = %d, want 4", len(p.conns))
	}
	for i := 0; i < 2; i++ {
		if p.conns[i] == nil {
			t.Fatalf("conns[%d] not populated", i)
		}
	}
	for i := 2; i < 4; i++ {
		if p.conns[i] != nil {
			t.Fatalf("conns[%d] unexpectedly populated", i)
		}
	}
	if p.shrinkable.Load() {
		t.Fatal("new pool must not be shrinkable")
	}

	c, err := p.Get()
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if c.Value() == nil {
		t.Fatal("Get returned nil connection")
	}
	_ = c.Close()
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for i, c := range p.conns {
		if c != nil {
			t.Fatalf("conns[%d] not detached after Close", i)
		}
	}
}

func TestGetNilConnReturnsErrClosed(t *testing.T) {
	p := newTestPool(t, singleConnOptions(), 1)
	p.conns[0] = nil

	_, err := p.Get()
	if err != ErrClosed {
		t.Fatalf("expected ErrClosed for nil slot, got %v", err)
	}
	if ref := p.ref.Load(); ref != 0 {
		t.Fatalf("expected ref 0, got %d", ref)
	}
}

func TestGetDialFailureNoRefLeak(t *testing.T) {
	dialErr := errors.New("dial failure")

	p := newTestPool(t, Options{
		OptionType:           ConnPool,
		MaxIdle:              1,
		MaxActive:            2,
		MaxConcurrentStreams: 1,
		Reuse:                false,
	}, 2)
	p.dialFn = func() (*grpc.ClientConn, error) { return nil, dialErr }
	p.ref.Store(2)

	if _, err := p.Get(); err != dialErr {
		t.Fatalf("expected dialErr, got %v", err)
	}
	if ref := p.ref.Load(); ref != 2 {
		t.Fatalf("expected ref 2 after dedicated dial failure, got %d", ref)
	}

	p2 := newTestPool(t, Options{
		OptionType:           ConnPool,
		MaxIdle:              1,
		MaxActive:            4,
		MaxConcurrentStreams: 1,
		Reuse:                true,
	}, 1)
	p2.dialFn = func() (*grpc.ClientConn, error) { return nil, dialErr }
	p2.ref.Store(1)

	if _, err := p2.Get(); err != dialErr {
		t.Fatalf("expected dialErr, got %v", err)
	}
	if ref := p2.ref.Load(); ref != 1 {
		t.Fatalf("expected ref 1 after expansion dial failure, got %d", ref)
	}
}

func TestDialReturnsNilNil(t *testing.T) {
	p := newTestPool(t, Options{
		OptionType:           ConnPool,
		MaxIdle:              1,
		MaxActive:            1,
		MaxConcurrentStreams: 1,
		Reuse:                false,
	}, 1)
	p.ref.Store(1)
	p.dialFn = func() (*grpc.ClientConn, error) { return nil, nil }

	if _, err := p.Get(); !errors.Is(err, errNilDialConn) {
		t.Fatalf("expected errNilDialConn, got %v", err)
	}
	if ref := p.ref.Load(); ref != 1 {
		t.Fatalf("expected ref 1 after nil dial, got %d", ref)
	}
}

func TestDialClosesConnOnError(t *testing.T) {
	p := newTestPool(t, singleConnOptions(), 1)
	var dialed *grpc.ClientConn
	p.dialFn = func() (*grpc.ClientConn, error) {
		cc, err := grpc.Dial(testTarget, grpc.WithInsecure())
		if err != nil {
			return nil, err
		}
		dialed = cc
		return cc, errors.New("boom")
	}

	if _, err := p.dial(); err == nil {
		t.Fatal("expected dial error")
	}
	if dialed == nil {
		t.Fatal("dial did not return a connection")
	}
	if dialed.GetState() != connectivity.Shutdown {
		t.Fatalf("connection returned with an error was not closed: %v", dialed.GetState())
	}
}

func TestClosedConnReturnsRPCError(t *testing.T) {
	cc, err := grpc.Dial(testTarget, grpc.WithInsecure())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := cc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	err = cc.Invoke(context.Background(), "/probe/Noop", &emptypb.Empty{}, &emptypb.Empty{})
	if err == nil {
		t.Fatal("expected an RPC error on a closed ClientConn")
	}
	if code := status.Code(err); code != codes.Canceled {
		t.Fatalf("closed ClientConn RPC code = %v, want %v (err: %v)", code, codes.Canceled, err)
	}
}

func TestSingleConnWithLargerMaxActive(t *testing.T) {
	p := newTestPool(t, Options{
		OptionType: SingleConn,
		MaxIdle:    1,
		MaxActive:  4,
		Reuse:      true,
	}, 1)

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !p.closed {
		t.Fatal("expected closed")
	}
	if len(p.conns) != 1 || p.conns[0] != nil {
		t.Fatalf("expected one detached slot, got %+v", p.conns)
	}
}

func TestDecrRefShrinkPath(t *testing.T) {
	p := newTestPool(t, Options{
		OptionType:           ConnPool,
		MaxIdle:              2,
		MaxActive:            4,
		MaxConcurrentStreams: 1,
		Reuse:                true,
	}, 4)
	evicted := []*grpc.ClientConn{p.conns[2].cc, p.conns[3].cc}
	p.ref.Store(1)

	p.decrRef()

	if ref := p.ref.Load(); ref != 0 {
		t.Fatalf("expected ref 0, got %d", ref)
	}
	if p.current != 2 {
		t.Fatalf("expected current 2, got %d", p.current)
	}
	for i := 2; i < 4; i++ {
		if p.conns[i] != nil {
			t.Fatalf("expected conns[%d] nil after shrink", i)
		}
	}
	for _, cc := range evicted {
		if cc.GetState() != connectivity.Shutdown {
			t.Fatalf("expected evicted conn to be shut down, got %v", cc.GetState())
		}
	}
}

func TestDecrRefRedundantDoesNotShrink(t *testing.T) {
	p := newTestPool(t, Options{
		OptionType:           ConnPool,
		MaxIdle:              2,
		MaxActive:            4,
		MaxConcurrentStreams: 1,
		Reuse:                true,
	}, 4)

	p.decrRef()

	if p.current != 4 {
		t.Fatalf("redundant decrRef must not shrink, current = %d", p.current)
	}
	for i, c := range p.conns {
		if c == nil {
			t.Fatalf("redundant decrRef must not detach conns[%d]", i)
		}
	}
}

func TestCloseVsShrinkDoesNotResurrect(t *testing.T) {
	p := newTestPool(t, Options{
		OptionType:           ConnPool,
		MaxIdle:              2,
		MaxActive:            4,
		MaxConcurrentStreams: 1,
		Reuse:                true,
	}, 4)
	p.ref.Store(1)

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	p.decrRef()

	if !p.closed {
		t.Fatal("expected closed to remain true")
	}
	if p.current != 0 {
		t.Fatalf("shrink resurrected current to %d", p.current)
	}
	for i, c := range p.conns {
		if c != nil {
			t.Fatalf("expected conns[%d] nil", i)
		}
	}
}

func TestOwnedLeaseAfterCloseReturnsErrClosed(t *testing.T) {
	p := newTestPool(t, Options{
		OptionType:           ConnPool,
		MaxIdle:              1,
		MaxActive:            1,
		MaxConcurrentStreams: 1,
		Reuse:                false,
	}, 1)
	p.ref.Store(1)

	dialStarted := make(chan struct{})
	release := make(chan struct{})
	var dialed *grpc.ClientConn
	var once sync.Once
	p.dialFn = func() (*grpc.ClientConn, error) {
		once.Do(func() { close(dialStarted) })
		<-release
		cc, err := grpc.Dial(testTarget, grpc.WithInsecure())
		dialed = cc
		return cc, err
	}

	done := make(chan error, 1)
	go func() {
		_, err := p.Get()
		done <- err
	}()

	<-dialStarted
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	close(release)

	if err := <-done; err != ErrClosed {
		t.Fatalf("expected ErrClosed, got %v", err)
	}
	if dialed == nil {
		t.Fatal("dial did not return a connection")
	}
	if dialed.GetState() != connectivity.Shutdown {
		t.Fatalf("expected newly dialed conn to be closed, got %v", dialed.GetState())
	}
	if ref := p.ref.Load(); ref != 1 {
		t.Fatalf("expected the pre-existing ref to remain, got %d", ref)
	}
}

func TestCloseWaitsForExpansionDial(t *testing.T) {
	p := newTestPool(t, Options{
		OptionType:           ConnPool,
		MaxIdle:              1,
		MaxActive:            4,
		MaxConcurrentStreams: 1,
		Reuse:                true,
	}, 1)
	p.ref.Store(1)

	dialStarted := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	p.dialFn = func() (*grpc.ClientConn, error) {
		once.Do(func() { close(dialStarted) })
		<-release
		return grpc.Dial(testTarget, grpc.WithInsecure())
	}

	type getResult struct {
		c   Conn
		err error
	}
	getDone := make(chan getResult, 1)
	go func() {
		c, err := p.Get()
		getDone <- getResult{c: c, err: err}
	}()

	<-dialStarted
	if p.mu.TryLock() {
		p.mu.Unlock()
		t.Fatal("expansion dial should hold the write lock")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- p.Close() }()

	select {
	case <-closeDone:
		t.Fatal("Close returned before expansion dial released")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	res := <-getDone
	if err := <-closeDone; err != nil {
		t.Fatalf("Close: %v", err)
	}
	if (res.c != nil) != (res.err == nil) {
		t.Fatalf("Get returned inconsistent result: conn=%v err=%v", res.c, res.err)
	}
	if res.c != nil {
		if res.c.Value() == nil {
			t.Fatal("lease.Value is nil after concurrent Close")
		}
		_ = res.c.Close()
	}
}

func TestOwnedLeaseSuccess(t *testing.T) {
	p := newTestPool(t, Options{
		OptionType:           ConnPool,
		MaxIdle:              1,
		MaxActive:            1,
		MaxConcurrentStreams: 1,
		Reuse:                false,
	}, 1)

	held, err := p.Get()
	if err != nil {
		t.Fatalf("held Get: %v", err)
	}

	owned, err := p.Get()
	if err != nil {
		t.Fatalf("owned Get: %v", err)
	}
	l, ok := owned.(*lease)
	if !ok || !l.owned {
		t.Fatalf("expected an owned lease, got %#v", owned)
	}
	cc := owned.Value()
	if cc == nil {
		t.Fatal("owned lease Value is nil")
	}

	_ = owned.Close()
	_ = owned.Close()
	if cc.GetState() != connectivity.Shutdown {
		t.Fatalf("owned conn was not closed: %v", cc.GetState())
	}
	if ref := p.ref.Load(); ref != 1 {
		t.Fatalf("expected ref 1 after owned release, got %d", ref)
	}

	_ = held.Close()
	if ref := p.ref.Load(); ref != 0 {
		t.Fatalf("expected ref 0, got %d", ref)
	}
}

func TestOwnedLeaseSurvivesPoolClose(t *testing.T) {
	p := newTestPool(t, Options{
		OptionType:           ConnPool,
		MaxIdle:              1,
		MaxActive:            1,
		MaxConcurrentStreams: 1,
		Reuse:                false,
	}, 1)

	held, err := p.Get()
	if err != nil {
		t.Fatalf("held Get: %v", err)
	}
	owned, err := p.Get()
	if err != nil {
		t.Fatalf("owned Get: %v", err)
	}
	cc := owned.Value()
	if cc == nil {
		t.Fatal("owned lease Value is nil")
	}

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if owned.Value() == nil {
		t.Fatal("owned lease Value became nil after pool.Close")
	}
	if cc.GetState() == connectivity.Shutdown {
		t.Fatal("pool.Close must not close a dedicated owned connection")
	}

	_ = owned.Close()
	if cc.GetState() != connectivity.Shutdown {
		t.Fatalf("owned lease did not close its connection: %v", cc.GetState())
	}
	_ = held.Close()
}

func TestConcurrentGetCloseWithConnPool(t *testing.T) {
	p := newTestPool(t, Options{
		OptionType:           ConnPool,
		MaxIdle:              2,
		MaxActive:            8,
		MaxConcurrentStreams: 2,
		Reuse:                true,
	}, 2)

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 100; j++ {
				c, err := p.Get()
				if err != nil {
					continue
				}
				if c.Value() == nil {
					t.Errorf("nil connection from ConnPool Get")
					return
				}
				_ = c.Close()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		_ = p.Close()
	}()

	close(start)
	wg.Wait()
	_ = p.Close()

	if ref := p.ref.Load(); ref != 0 {
		t.Fatalf("expected ref 0 after workers finish, got %d", ref)
	}
}

func TestActiveTimeAdvances(t *testing.T) {
	p := newTestPool(t, singleConnOptions(), 1)
	before, _ := p.GetActiveTimeAndRef()

	time.Sleep(5 * time.Millisecond)
	c, err := p.Get()
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	after, _ := p.GetActiveTimeAndRef()
	if !after.After(before) {
		t.Fatalf("active time did not advance: before=%v after=%v", before, after)
	}
	_ = c.Close()
}

func TestActiveTimeMonotonic(t *testing.T) {
	p := newTestPool(t, singleConnOptions(), 1)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				c, err := p.Get()
				if err == nil {
					_ = c.Close()
				}
			}
		}()
	}

	var prev time.Time
	for i := 0; i < 1000; i++ {
		active, _ := p.GetActiveTimeAndRef()
		if active.Before(prev) {
			t.Fatalf("active time went backwards: %v -> %v", prev, active)
		}
		prev = active
	}
	wg.Wait()
}

func TestIncrRefOverflowProtection(t *testing.T) {
	p := newTestPool(t, singleConnOptions(), 1)
	p.ref.Store(math.MaxInt32 - 1)

	ref, ok := p.incrRef()
	if !ok || ref != math.MaxInt32 {
		t.Fatalf("expected successful increment to MaxInt32, got ref=%d ok=%v", ref, ok)
	}
	if ref, ok = p.incrRef(); ok {
		t.Fatalf("expected saturation at MaxInt32, got ref=%d ok=%v", ref, ok)
	}

	if _, err := p.Get(); err != ErrRefOverflow {
		t.Fatalf("expected ErrRefOverflow, got %v", err)
	}
	if ref := p.ref.Load(); ref != math.MaxInt32 {
		t.Fatalf("expected ref to stay at MaxInt32, got %d", ref)
	}
}

func TestDecrRefUnderflow(t *testing.T) {
	p := &pool{current: 1}

	p.decrRef()
	if ref := p.ref.Load(); ref < 0 {
		t.Fatalf("expected ref >= 0, got %d", ref)
	}
	for i := 0; i < 5; i++ {
		p.decrRef()
	}
	if ref := p.ref.Load(); ref != 0 {
		t.Fatalf("expected ref 0, got %d", ref)
	}
	if ref, ok := p.incrRef(); !ok || ref != 1 {
		t.Fatalf("expected incrRef to return 1, got ref=%d ok=%v", ref, ok)
	}
}

func TestConcurrentIncrDecrRef(t *testing.T) {
	p := &pool{}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				if _, ok := p.incrRef(); !ok {
					t.Errorf("incrRef saturated unexpectedly")
					return
				}
				if cur := p.ref.Load(); cur < 0 {
					t.Errorf("ref underflow: %d", cur)
				}
				p.decrRef()
				if cur := p.ref.Load(); cur < 0 {
					t.Errorf("ref underflow: %d", cur)
				}
			}
		}()
	}
	wg.Wait()

	if ref := p.ref.Load(); ref != 0 {
		t.Fatalf("expected ref 0, got %d", ref)
	}
}

func TestGracefulStopImmediateWhenZeroRef(t *testing.T) {
	p := newTestPool(t, singleConnOptions(), 1)

	start := time.Now()
	p.GracefulStop(1 * time.Second)
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("GracefulStop took %v with ref=0", elapsed)
	}
	if !p.closed {
		t.Fatal("expected pool closed")
	}
}

func TestGracefulStopTimeout(t *testing.T) {
	p := newTestPool(t, singleConnOptions(), 1)
	p.ref.Store(2)

	start := time.Now()
	p.GracefulStop(50 * time.Millisecond)
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("GracefulStop waited unexpected duration: %v", elapsed)
	}
	if !p.closed {
		t.Fatal("expected pool closed")
	}
}

func BenchmarkGetLease(b *testing.B) {
	p := newTestPool(b, singleConnOptions(), 1)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c, err := p.Get()
		if err != nil {
			b.Fatal(err)
		}
		_ = c.Close()
	}
}

func BenchmarkGetLeaseParallel(b *testing.B) {
	p := newTestPool(b, singleConnOptions(), 1)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			c, err := p.Get()
			if err != nil {
				b.Error(err)
				return
			}
			_ = c.Close()
		}
	})
}

func BenchmarkGetLeaseConnPoolAtCapacity(b *testing.B) {
	p := newTestPool(b, Options{
		OptionType:           ConnPool,
		MaxIdle:              1,
		MaxActive:            1,
		MaxConcurrentStreams: 1,
		Reuse:                true,
	}, 1)
	held, err := p.Get()
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		_ = held.Close()
		_ = p.Close()
	})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c, err := p.Get()
		if err != nil {
			b.Fatal(err)
		}
		_ = c.Close()
	}
}
