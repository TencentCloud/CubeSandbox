// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package pool implements the internal gRPC connection pool shared by
// CubeMaster and Cubelet.
//
// A pool owns a fixed set of slots (poolConn). Every successful Get returns a
// lease: an immutable snapshot of the underlying *grpc.ClientConn plus an
// idempotent release of the pool reference. Callers never observe pool-owned
// mutable state, so a concurrent Close can only surface as an RPC error, never
// as a nil connection.
package pool

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	ErrClosed = errors.New("pool is closed")
	// ErrRefOverflow is returned when the pool cannot reserve another
	// reference because the counter is saturated. This is unreachable in
	// practice (it needs math.MaxInt32 outstanding references), but it keeps
	// the counter and the "reference acquired" contract consistent.
	ErrRefOverflow = errors.New("pool reference count overflow")

	errNilDialConn = errors.New("grpc pool: dial returned nil connection")
	errNilDialFn   = errors.New("grpc pool: nil dial function")
)

type Pool interface {
	Get() (Conn, error)

	GetActiveTimeAndRef() (time.Time, int32)

	Close() error

	GracefulStop(maxWaitTime time.Duration)

	Status() string
}

// pool is the shared implementation. mu protects conns, closed and current;
// ref, index and activeNano are atomics so the hot path never takes the write
// lock. timeBase is immutable after newPool.
type pool struct {
	mu      sync.RWMutex
	closed  bool
	conns   []*poolConn
	current int32

	ref   atomic.Int32
	index atomic.Uint32

	// shrinkable is true only while current > MaxIdle, i.e. after an
	// expansion. It lets decrRef skip the write lock on the common
	// SingleConn release path, where no shrink is ever possible.
	shrinkable atomic.Bool

	activeNano atomic.Int64
	timeBase   time.Time

	opt     Options
	address string
	dialFn  func() (*grpc.ClientConn, error)
	// closeFn is the teardown hook. It is set once during construction,
	// defaults to closePoolConns, and is injectable for tests that need to
	// observe Close completion. It must not be mutated after construction.
	closeFn func([]*poolConn) error

	closeOnce sync.Once
	closeErr  error
}

// newPool builds the module-independent state. newPoolWithSlots performs the
// constructor validation and initial slot dialing; module-specific New only
// adapts the dialer signature and DNS rewriting.
func newPool(address string, opt Options, dialFn func() (*grpc.ClientConn, error)) *pool {
	p := &pool{
		address:  address,
		opt:      opt,
		dialFn:   dialFn,
		closeFn:  closePoolConns,
		timeBase: time.Now(),
	}
	p.touchActive()
	return p
}

// validatePoolConfig checks the construction rules that do not depend on the
// module-specific Dial signature. New in new.go checks Dial itself and then
// delegates here, so both modules enforce the same capacity contract.
func validatePoolConfig(address string, opt Options) error {
	if address == "" {
		return errors.New("invalid address settings")
	}
	if opt.MaxIdle <= 0 || opt.MaxActive <= 0 || opt.MaxIdle > opt.MaxActive {
		return errors.New("invalid maximum settings")
	}
	if opt.OptionType != SingleConn && opt.MaxConcurrentStreams <= 0 {
		return errors.New("invalid maximum settings")
	}
	return nil
}

// newPoolWithSlots builds a pool and dials its initial slots. Keeping the
// construction body here makes the lifecycle code shared verbatim by
// CubeMaster and Cubelet; module-specific New only supplies the dial adapter.
func newPoolWithSlots(address string, opt Options, dialFn func() (*grpc.ClientConn, error)) (*pool, error) {
	if err := validatePoolConfig(address, opt); err != nil {
		return nil, err
	}

	p := newPool(address, opt, dialFn)
	if opt.OptionType == SingleConn {
		p.conns = make([]*poolConn, 1)
		p.current = 1
		cc, err := p.dial()
		if err != nil {
			_ = p.Close()
			return nil, err
		}
		p.conns[0] = &poolConn{cc: cc}
		return p, nil
	}

	p.conns = make([]*poolConn, opt.MaxActive)
	p.current = int32(opt.MaxIdle)
	for i := 0; i < opt.MaxIdle; i++ {
		cc, err := p.dial()
		if err != nil {
			_ = p.Close()
			return nil, fmt.Errorf("dial [%s] is not able to fill the pool: %w", address, err)
		}
		p.conns[i] = &poolConn{cc: cc}
	}
	return p, nil
}

// setCurrentLocked updates current and its derived shrinkable flag together.
// The caller must hold p.mu for writing. Keeping the two values in one helper
// prevents an expansion/shrink/Close path from forgetting one of them.
func (p *pool) setCurrentLocked(v int32) {
	p.current = v
	p.shrinkable.Store(v > int32(p.opt.MaxIdle))
}

// touchActive records the current time without taking mu. CAS-max keeps the
// value monotonic even when concurrent Gets are preempted between reading the
// clock and storing it.
func (p *pool) touchActive() {
	if p.timeBase.IsZero() {
		return
	}
	n := int64(time.Since(p.timeBase))
	for {
		old := p.activeNano.Load()
		if n <= old {
			return
		}
		if p.activeNano.CompareAndSwap(old, n) {
			return
		}
	}
}

func (p *pool) GetActiveTimeAndRef() (time.Time, int32) {
	if p.timeBase.IsZero() {
		return time.Time{}, p.ref.Load()
	}
	return p.timeBase.Add(time.Duration(p.activeNano.Load())), p.ref.Load()
}

// incrRef reserves one reference and reports whether the reservation
// succeeded. When the counter is saturated no reference is reserved, so the
// caller must not hand out a connection.
func (p *pool) incrRef() (int32, bool) {
	for {
		old := p.ref.Load()
		if old >= math.MaxInt32 {
			return old, false
		}
		if p.ref.CompareAndSwap(old, old+1) {
			return old + 1, true
		}
	}
}

func (p *pool) decrRef() {
	if p == nil {
		return
	}

	// Only a real 1 -> 0 transition may shrink the pool. A redundant
	// decrement (ref already 0) must not change current or tear down conns.
	reachedZero := false
	for {
		old := p.ref.Load()
		if old <= 0 {
			break
		}
		if p.ref.CompareAndSwap(old, old-1) {
			reachedZero = old == 1
			break
		}
	}
	if !reachedZero || !p.shrinkable.Load() {
		return
	}

	p.mu.Lock()
	var detached []*poolConn
	if !p.closed && p.ref.Load() == 0 && p.current > int32(p.opt.MaxIdle) {
		p.setCurrentLocked(int32(p.opt.MaxIdle))
		detached = p.detachFromLocked(p.opt.MaxIdle)
	}
	p.mu.Unlock()
	_ = p.closeDetached(detached)
}

// detachFromLocked nil's the slots in [begin, len(conns)) and returns the
// detached slots. It must be called with p.mu held for writing. Iterating over
// len(conns) is intentional: SingleConn allocates exactly one slot even when
// MaxActive is larger.
func (p *pool) detachFromLocked(begin int) []*poolConn {
	if begin < 0 {
		begin = 0
	}
	if begin > len(p.conns) {
		begin = len(p.conns)
	}
	var detached []*poolConn
	for i := begin; i < len(p.conns); i++ {
		if c := p.conns[i]; c != nil {
			detached = append(detached, c)
			p.conns[i] = nil
		}
	}
	return detached
}

func closePoolConns(conns []*poolConn) error {
	var errs []error
	for _, c := range conns {
		if err := c.close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (p *pool) closeDetached(conns []*poolConn) error {
	if len(conns) == 0 {
		return nil
	}
	if p.closeFn != nil {
		return p.closeFn(conns)
	}
	return closePoolConns(conns)
}

// dial centralizes the injectable dialer contract: a non-nil error wins, a
// returned connection is closed on error, and a nil connection without an
// error is rejected.
func (p *pool) dial() (*grpc.ClientConn, error) {
	if p.dialFn == nil {
		return nil, errNilDialFn
	}
	cc, err := p.dialFn()
	if err != nil {
		if cc != nil {
			_ = cc.Close()
		}
		return nil, err
	}
	if cc == nil {
		return nil, errNilDialConn
	}
	return cc, nil
}

// leaseFromSlot snapshots an immutable slot. It must be called without mu
// held; it never touches pool-owned mutable state.
func (p *pool) leaseFromSlot(slot *poolConn) (Conn, error) {
	if slot == nil || slot.cc == nil {
		p.decrRef()
		return nil, ErrClosed
	}
	return p.newLease(slot.cc, false)
}

// newLease builds a caller-facing acquisition. The caller must already hold a
// reference. A nil connection is rejected and the reference is released.
func (p *pool) newLease(cc *grpc.ClientConn, owned bool) (Conn, error) {
	if cc == nil {
		p.decrRef()
		return nil, errNilDialConn
	}
	return &lease{cc: cc, pool: p, owned: owned}, nil
}

// leaseNext takes a slot from the current pool. It re-reads current under the
// read lock and computes the index there; callers must not hold mu.
func (p *pool) leaseNext() (Conn, error) {
	p.mu.RLock()
	if p.closed || p.current <= 0 {
		p.mu.RUnlock()
		p.decrRef()
		return nil, ErrClosed
	}
	index := int(p.index.Add(1) % uint32(p.current))
	slot := p.conns[index]
	p.mu.RUnlock()
	return p.leaseFromSlot(slot)
}

func (p *pool) Get() (Conn, error) {
	defer p.touchActive()

	p.mu.RLock()
	if p.closed || p.current <= 0 {
		p.mu.RUnlock()
		return nil, ErrClosed
	}
	nextRef, ok := p.incrRef()
	if !ok {
		p.mu.RUnlock()
		return nil, ErrRefOverflow
	}
	current := p.current

	var index int
	fast := true
	switch {
	case p.opt.OptionType == SingleConn:
		index = 0
	case nextRef <= int32(p.opt.MaxIdle)*int32(p.opt.MaxConcurrentStreams):
		index = int(p.index.Add(1) % uint32(p.opt.MaxIdle))
	case nextRef <= current*int32(p.opt.MaxConcurrentStreams):
		index = int(p.index.Add(1) % uint32(current))
	case current >= int32(p.opt.MaxActive) && p.opt.Reuse:
		// A saturated reusable pool must stay on the read-lock fast path;
		// falling through would take the write lock on every excess Get.
		index = int(p.index.Add(1) % uint32(current))
	default:
		fast = false
	}

	if fast {
		slot := p.conns[index]
		p.mu.RUnlock()
		return p.leaseFromSlot(slot)
	}

	needDial := current >= int32(p.opt.MaxActive)
	p.mu.RUnlock()

	if needDial {
		cc, err := p.dial()
		if err != nil {
			p.decrRef()
			return nil, err
		}
		// A dedicated connection is not tracked in conns, so synchronize the
		// final closed check with Close before returning it.
		p.mu.RLock()
		closed := p.closed
		p.mu.RUnlock()
		if closed {
			_ = cc.Close()
			p.decrRef()
			return nil, ErrClosed
		}
		return p.newLease(cc, true)
	}

	// Expansion. Dialing under the write lock is pre-existing behavior; Close
	// waits for the dial to finish (bounded by the dial timeout).
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		p.decrRef()
		return nil, ErrClosed
	}
	current = p.current
	var stale []*poolConn
	if current < int32(p.opt.MaxActive) && nextRef > current*int32(p.opt.MaxConcurrentStreams) {
		increment := current
		if current+increment > int32(p.opt.MaxActive) {
			increment = int32(p.opt.MaxActive) - current
		}
		var i int32
		var err error
		for i = 0; i < increment; i++ {
			cc, er := p.dial()
			if er != nil {
				err = er
				break
			}
			idx := int(current + i)
			if old := p.conns[idx]; old != nil {
				// Defensive only: slots at or above current are expected to
				// be detached already.
				stale = append(stale, old)
			}
			p.conns[idx] = &poolConn{cc: cc}
		}
		current += i
		p.setCurrentLocked(current)
		if err != nil {
			p.mu.Unlock()
			_ = p.closeDetached(stale)
			p.decrRef()
			return nil, err
		}
	}
	p.mu.Unlock()
	_ = p.closeDetached(stale)
	return p.leaseNext()
}

func (p *pool) Close() error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.index.Store(0)
		p.setCurrentLocked(0)
		detached := p.detachFromLocked(0)
		p.mu.Unlock()
		p.closeErr = p.closeDetached(detached)
	})
	return p.closeErr
}

func (p *pool) GracefulStop(maxWaitTime time.Duration) {
	timer := time.NewTimer(maxWaitTime)
	defer timer.Stop()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		if p.ref.Load() <= 0 {
			_ = p.Close()
			return
		}
		select {
		case <-timer.C:
			_ = p.Close()
			return
		case <-ticker.C:
		}
	}
}

func (p *pool) Status() string {
	p.mu.RLock()
	current := p.current
	closed := p.closed
	p.mu.RUnlock()
	return fmt.Sprintf("address:%s, index:%d, current:%d, ref:%d, closed:%t. option:%v",
		p.address, p.index.Load(), current, p.ref.Load(), closed, p.opt)
}

func (p *pool) IsNetErr(err error) bool {
	if err != nil && status.Code(err) == codes.Unavailable {
		return true
	}

	return false
}
