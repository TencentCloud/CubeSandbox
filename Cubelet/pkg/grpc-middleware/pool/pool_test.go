// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package pool

// The full lifecycle suite lives in CubeMaster, where the pool is used in
// production. Cubelet keeps a smoke subset for its pinned gRPC version plus
// module-specific New/DNS tests; conn.go and pool.go are byte-identical and
// kept manually in sync.

import (
	"context"
	"sync"
	"testing"

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

func TestLeaseValueStableAfterPoolClose(t *testing.T) {
	p, err := newPoolWithSlots("test-address", Options{
		OptionType: SingleConn,
		MaxIdle:    1,
		MaxActive:  1,
		Reuse:      true,
	}, testDialer(t))
	if err != nil {
		t.Fatalf("newPoolWithSlots: %v", err)
	}

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
