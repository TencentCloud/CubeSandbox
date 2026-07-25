// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package filter

import (
	"context"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/scarceresource"
)

func TestScarceResourceFilterSelect(t *testing.T) {
	restore := scarceresource.SetEffectiveResourcesFnForTest(func() []config.ScarceResourceDef {
		return []config.ScarceResourceDef{{LabelKey: "gpu", LabelValues: []string{"true"}}}
	})
	defer restore()

	ctx := selctx.New("random")
	ctx.Ctx = context.Background()
	ctx.SetNodes(node.NodeList{
		{InsID: "gpu-node", NodeLabels: map[string]string{"gpu": "true"}},
		{InsID: "cpu-node"},
	})

	got, err := NewScarceResourceFilter().Select(ctx)
	if err != nil {
		t.Fatalf("Select returned error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 node, got %d", len(got))
	}
	if got[0].ID() != "cpu-node" {
		t.Fatalf("expected cpu-node, got %s", got[0].ID())
	}
}
