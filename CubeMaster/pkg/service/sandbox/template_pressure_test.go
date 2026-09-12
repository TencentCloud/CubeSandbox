// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

// TestTrackTemplateCreateIncrDecr 选中节点后 +1、归还函数调用后 -1
func TestTrackTemplateCreateIncrDecr(t *testing.T) {
	const (
		nodeID     = "node-track-tpl"
		templateID = "tpl-track"
	)
	t.Cleanup(func() {
		for localcache.NodeTemplateCreateNum(nodeID, templateID) > 0 {
			localcache.DecrNodeTemplateCreate(nodeID, templateID)
		}
	})

	host := &node.Node{InsID: nodeID}
	selCtx := selctx.New("random")
	selCtx.ReqRes = &selctx.RequestResource{TemplateID: templateID}

	untrack := trackTemplateCreate(host, selCtx)
	if got := localcache.NodeTemplateCreateNum(nodeID, templateID); got != 1 {
		t.Fatalf("count after track = %d, want 1", got)
	}

	untrack()
	if got := localcache.NodeTemplateCreateNum(nodeID, templateID); got != 0 {
		t.Fatalf("count after untrack = %d, want 0", got)
	}
}

// TestTrackTemplateCreateNoop 无模板 ID 或空输入时不登记任何计数
func TestTrackTemplateCreateNoop(t *testing.T) {
	host := &node.Node{InsID: "node-track-noop"}

	cases := []struct {
		name   string
		host   *node.Node
		selCtx *selctx.SelectorCtx
	}{
		{"nil host", nil, &selctx.SelectorCtx{ReqRes: &selctx.RequestResource{TemplateID: "tpl-x"}}},
		{"nil selctx", host, nil},
		{"nil reqres", host, &selctx.SelectorCtx{}},
		{"empty template", host, &selctx.SelectorCtx{ReqRes: &selctx.RequestResource{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			untrack := trackTemplateCreate(tc.host, tc.selCtx)
			untrack() // 必须是安全的空操作
			if got := localcache.NodeTemplateCreateNum("node-track-noop", "tpl-x"); got != 0 {
				t.Fatalf("count = %d, want 0", got)
			}
		})
	}
}
