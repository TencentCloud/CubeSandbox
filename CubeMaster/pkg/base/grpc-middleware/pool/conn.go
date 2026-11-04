// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package pool

import (
	"sync"

	"google.golang.org/grpc"
)

// Conn is a single acquisition from the pool. Value returns the connection
// captured by that acquisition; it is never nil for a successful Get, even if
// the pool is closed concurrently. Close releases the acquisition exactly once.
type Conn interface {
	Value() *grpc.ClientConn

	Close() error
}

// poolConn is a pool-owned slot. cc is immutable after construction. The pool
// detaches a slot at most once (under its write lock) and then closes it.
type poolConn struct {
	cc *grpc.ClientConn
}

func (c *poolConn) close() error {
	if c == nil || c.cc == nil {
		return nil
	}
	return c.cc.Close()
}

// lease is one Get acquisition. It snapshots cc so callers never alias mutable
// pool state: a concurrent pool.Close can close the underlying connection, but
// it can never turn lease.cc into nil.
type lease struct {
	cc   *grpc.ClientConn
	pool *pool
	// owned is true for a dedicated connection dialed outside the slot set
	// (ConnPool with Reuse=false). Such a lease closes cc on release; pooled
	// leases only release the reference and leave the slot to the pool.
	owned bool

	closeOnce sync.Once
	closeErr  error
}

func (l *lease) Value() *grpc.ClientConn {
	return l.cc
}

func (l *lease) Close() error {
	l.closeOnce.Do(func() {
		if l.pool != nil {
			l.pool.decrRef()
		}
		if l.owned && l.cc != nil {
			l.closeErr = l.cc.Close()
		}
	})
	return l.closeErr
}
