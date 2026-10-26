// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package grpcconn

import (
	"context"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	grpcpool "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/grpc-middleware/pool"
)

type mockConn struct {
	grpcpool.Conn
}

type mockPool struct {
	closed bool
	ref    int32
	active time.Time
}

func (m *mockPool) Get() (grpcpool.Conn, error) {
	return &mockConn{}, nil
}

func (m *mockPool) GetActiveTimeAndRef() (time.Time, int32) {
	return m.active, m.ref
}

func (m *mockPool) Close() error {
	m.closed = true
	return nil
}

func (m *mockPool) GracefulStop(maxWaitTime time.Duration) {
	m.closed = true
}

func (m *mockPool) Status() string {
	return "mock"
}

func TestCloseWorkerConnKeyMismatch(t *testing.T) {
	oldPool := connPool
	t.Cleanup(func() { connPool = oldPool })

	pool1 := &mockPool{}
	pool2 := &mockPool{}
	pool3 := &mockPool{}
	poolOther := &mockPool{}

	connPool = &workerGrpcConnPool{
		ctx: context.Background(),
	}

	targetAddr := "192.168.1.100:12345"
	otherAddr := "192.168.1.101:12345"

	key1 := "ua1+" + targetAddr
	key2 := "ua2+" + targetAddr
	key3 := targetAddr
	keyOther := "ua1+" + otherAddr

	connPool.cache.Store(key1, pool1)
	connPool.cache.Store(key2, pool2)
	connPool.cache.Store(key3, pool3)
	connPool.cache.Store(keyOther, poolOther)

	// Close all connections for targetAddr
	CloseWorkerConn(targetAddr)

	if _, ok := connPool.cache.Load(key1); ok {
		t.Fatalf("expected key %s to be deleted from cache", key1)
	}
	if !pool1.closed {
		t.Fatalf("expected pool1 to be closed")
	}

	if _, ok := connPool.cache.Load(key2); ok {
		t.Fatalf("expected key %s to be deleted from cache", key2)
	}
	if !pool2.closed {
		t.Fatalf("expected pool2 to be closed")
	}

	if _, ok := connPool.cache.Load(key3); ok {
		t.Fatalf("expected key %s to be deleted from cache", key3)
	}
	if !pool3.closed {
		t.Fatalf("expected pool3 to be closed")
	}

	// Other address should remain intact
	if _, ok := connPool.cache.Load(keyOther); !ok {
		t.Fatalf("expected key %s to remain in cache", keyOther)
	}
	if poolOther.closed {
		t.Fatalf("expected poolOther to remain open")
	}
}

func TestCloseWorkerConnNilPool(t *testing.T) {
	oldPool := connPool
	t.Cleanup(func() { connPool = oldPool })

	connPool = nil
	// Should not panic
	CloseWorkerConn("192.168.1.100:12345")
}

func TestGetWorkerConnUninitialized(t *testing.T) {
	oldPool := connPool
	t.Cleanup(func() { connPool = oldPool })

	connPool = nil
	ctx := context.Background()
	_, err := GetWorkerConn(ctx, "192.168.1.100:12345")
	if err == nil {
		t.Fatalf("expected error when connPool is uninitialized")
	}
}

func TestGetWorkerConnCached(t *testing.T) {
	oldPool := connPool
	t.Cleanup(func() { connPool = oldPool })

	connPool = &workerGrpcConnPool{
		ctx: context.Background(),
	}

	ctx := constants.WithUA(context.Background(), "test-ua")
	addr := "192.168.1.200:12345"
	key := "test-ua+" + addr

	cachedPool := &mockPool{}
	connPool.cache.Store(key, cachedPool)

	conn, err := GetWorkerConn(ctx, addr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if conn == nil {
		t.Fatalf("expected non-nil conn from mockPool.Get()")
	}
}
