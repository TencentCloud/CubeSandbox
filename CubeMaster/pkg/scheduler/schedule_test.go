// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package scheduler

import (
	"fmt"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/profile"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
	sfilter "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/filter"
)

func TestShouldSkipBackoffForTemplate(t *testing.T) {
	origFilters := scheduler.filter
	defer func() {
		scheduler.filter = origFilters
	}()

	tests := []struct {
		name    string
		ctx     *selctx.SelectorCtx
		filters []sfilter.Selector
		want    bool
	}{
		{
			name: "nil selector context",
			ctx:  nil,
			filters: []sfilter.Selector{
				sfilter.NewTemplateLocalityFilter(),
			},
			want: false,
		},
		{
			name: "request without template",
			ctx: &selctx.SelectorCtx{
				ReqRes: &selctx.RequestResource{},
			},
			filters: []sfilter.Selector{
				sfilter.NewTemplateLocalityFilter(),
			},
			want: false,
		},
		{
			name: "request with template but filter disabled",
			ctx: &selctx.SelectorCtx{
				ReqRes: &selctx.RequestResource{TemplateID: "tpl-1"},
			},
			filters: nil,
			want:    false,
		},
		{
			name: "request with template and filter enabled",
			ctx: &selctx.SelectorCtx{
				ReqRes: &selctx.RequestResource{TemplateID: "tpl-1"},
			},
			filters: []sfilter.Selector{
				sfilter.NewTemplateLocalityFilter(),
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheduler.filter = tt.filters
			if got := shouldSkipBackoffForTemplate(tt.ctx); got != tt.want {
				t.Fatalf("shouldSkipBackoffForTemplate() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSelectNodeSpread(t *testing.T) {
	newNodes := func(mvmNums ...int64) node.NodeList {
		nodes := node.NodeList{}
		for i, mvmNum := range mvmNums {
			nodes.Append(&node.Node{Index: i + 1, InsID: fmt.Sprintf("n%d", i+1), MvmNum: mvmNum})
		}
		return nodes
	}

	t.Run("picks the least occupied node inside top_n", func(t *testing.T) {
		nodes := newNodes(5, 2, 9, 1)
		selCtx := selctx.New("")
		selCtx.SetNodes(nodes)
		selCtx.SetNodeScoreList(node.NodeScoreList{
			{InsID: "n1", OrigNode: nodes[0], MvmNum: 5, Score: 90},
			{InsID: "n2", OrigNode: nodes[1], MvmNum: 2, Score: 80},
			{InsID: "n3", OrigNode: nodes[2], MvmNum: 9, Score: 70},
			{InsID: "n4", OrigNode: nodes[3], MvmNum: 1, Score: 60},
		})
		pipeline := &profile.Pipeline{Selection: profile.SelectionSpread, TopN: 3}
		if got := selectNode(selCtx, pipeline); got == nil || got.ID() != "n2" {
			t.Fatalf("spread top_n=3 should pick n2 (least MvmNum inside top 3), got %v", got)
		}
	})

	t.Run("tie keeps score order", func(t *testing.T) {
		nodes := newNodes(3, 3, 1)
		selCtx := selctx.New("")
		selCtx.SetNodes(nodes)
		selCtx.SetNodeScoreList(node.NodeScoreList{
			{InsID: "n1", OrigNode: nodes[0], MvmNum: 3, Score: 90},
			{InsID: "n2", OrigNode: nodes[1], MvmNum: 3, Score: 80},
			{InsID: "n3", OrigNode: nodes[2], MvmNum: 1, Score: 70},
		})
		pipeline := &profile.Pipeline{Selection: profile.SelectionSpread, TopN: 2}
		if got := selectNode(selCtx, pipeline); got == nil || got.ID() != "n1" {
			t.Fatalf("spread tie should keep score order (n1), got %v", got)
		}
	})

	t.Run("top_n -1 spreads across all candidates", func(t *testing.T) {
		nodes := newNodes(5, 2, 9, 1)
		selCtx := selctx.New("")
		selCtx.SetNodes(nodes)
		selCtx.SetNodeScoreList(node.NodeScoreList{
			{InsID: "n1", OrigNode: nodes[0], MvmNum: 5, Score: 90},
			{InsID: "n2", OrigNode: nodes[1], MvmNum: 2, Score: 80},
			{InsID: "n3", OrigNode: nodes[2], MvmNum: 9, Score: 70},
			{InsID: "n4", OrigNode: nodes[3], MvmNum: 1, Score: 60},
		})
		pipeline := &profile.Pipeline{Selection: profile.SelectionSpread, TopN: -1}
		if got := selectNode(selCtx, pipeline); got == nil || got.ID() != "n4" {
			t.Fatalf("spread top_n=-1 should pick n4 (global minimum), got %v", got)
		}
	})

	t.Run("unscored candidates fall back to enumeration order", func(t *testing.T) {
		nodes := newNodes(7, 2, 5)
		selCtx := selctx.New("")
		selCtx.SetNodes(nodes)
		pipeline := &profile.Pipeline{Selection: profile.SelectionSpread, TopN: 2}
		if got := selectNode(selCtx, pipeline); got == nil || got.ID() != "n2" {
			t.Fatalf("spread without scores should pick n2 (least MvmNum inside first 2), got %v", got)
		}
	})

	t.Run("random and highest are unchanged", func(t *testing.T) {
		nodes := newNodes(5, 2)
		selCtx := selctx.New("")
		selCtx.SetNodes(nodes)
		if got := selectNode(selCtx, &profile.Pipeline{Selection: profile.SelectionHighest, TopN: 2}); got == nil || got.ID() != "n1" {
			t.Fatalf("highest should pick the first candidate, got %v", got)
		}
		if got := selectNode(selCtx, &profile.Pipeline{Selection: profile.SelectionRandom, TopN: 1}); got == nil || got.ID() != "n1" {
			t.Fatalf("random top_n=1 should pick the first candidate, got %v", got)
		}
	})
}
