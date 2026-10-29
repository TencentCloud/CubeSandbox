// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package filter

import (
	"context"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

func realtimeLimitPatches(t *testing.T, realGlobal, local, limit int64) *gomonkey.Patches {
	t.Helper()
	patches := gomonkey.NewPatches()
	patches.ApplyFunc(localcache.RealTimeCreateConcurrentLimit, func(*node.Node) int64 { return realGlobal })
	patches.ApplyFunc(localcache.LocalCreateConcurrentLimit, func(*node.Node) int64 { return local })
	patches.ApplyFunc(localcache.CreateConcurrentLimit, func(*node.Node) int64 { return limit })
	patches.ApplyFunc(localcache.HealthyMasterNodes, func() int64 { return 1 })
	return patches
}

func realtimeLimitContext() *selctx.SelectorCtx {
	ctx := selctx.New("random")
	ctx.Ctx = context.Background()
	ctx.SetNodes(node.NodeList{
		{InsID: "node-a", IP: "10.0.0.1"},
		{InsID: "node-b", IP: "10.0.0.2"},
	})
	return ctx
}

func TestRealtimeCreateLimitStampsRejectReasonWhenAllRejected(t *testing.T) {
	patches := realtimeLimitPatches(t, 100, 0, 10)
	defer patches.Reset()

	ctx := realtimeLimitContext()
	got, err := NewRealtimecreatelimit().Select(ctx)
	if err != nil {
		t.Fatalf("Select returned error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected every node rejected, got %v", got)
	}
	if reason := ctx.GetRejectReason(); reason != selctx.RejectReasonConcurrencyLimit {
		t.Fatalf("reject reason = %q, want %q", reason, selctx.RejectReasonConcurrencyLimit)
	}
}

func TestRealtimeCreateLimitDoesNotStampOnPartialReject(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	// node-a is at the concurrency limit, node-b is not.
	patches.ApplyFunc(localcache.RealTimeCreateConcurrentLimit, func(n *node.Node) int64 {
		if n.ID() == "node-a" {
			return 100
		}
		return 0
	})
	patches.ApplyFunc(localcache.LocalCreateConcurrentLimit, func(*node.Node) int64 { return 0 })
	patches.ApplyFunc(localcache.CreateConcurrentLimit, func(*node.Node) int64 { return 10 })
	patches.ApplyFunc(localcache.HealthyMasterNodes, func() int64 { return 1 })

	ctx := realtimeLimitContext()
	got, err := NewRealtimecreatelimit().Select(ctx)
	if err != nil {
		t.Fatalf("Select returned error: %v", err)
	}
	if len(got) != 1 || got[0].ID() != "node-b" {
		t.Fatalf("expected only node-b to remain, got %v", got)
	}
	if reason := ctx.GetRejectReason(); reason != "" {
		t.Fatalf("partial reject must not stamp a reason, got %q", reason)
	}
}
