// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package scheduler

import (
	"context"
	"errors"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
	sfilter "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/filter"
	sscore "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/score"
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

func TestRunScoreFilterSkipsFailedScorers(t *testing.T) {
	origPostScore := scheduler.postScore
	defer func() {
		scheduler.postScore = origPostScore
	}()
	scheduler.postScore = nil

	nodeA := &node.Node{InsID: "node-a", MvmNum: 1}
	nodeB := &node.Node{InsID: "node-b", MvmNum: 2}
	selCtx := selctx.New("random")
	selCtx.Ctx = context.Background()
	selCtx.SetNodes(node.NodeList{nodeA, nodeB})

	err := runScoreFilter(selCtx, []sscore.Selector{
		testScoreSelector{
			err: errors.New("external scorer unavailable"),
		},
		testScoreSelector{
			weight: 2,
			scores: node.NodeScoreList{
				{InsID: "node-a", Score: 10, MvmNum: nodeA.MvmNum, OrigNode: nodeA},
				{InsID: "node-b", Score: 90, MvmNum: nodeB.MvmNum, OrigNode: nodeB},
			},
		},
	})
	if err != nil {
		t.Fatalf("runScoreFilter() error = %v, want nil", err)
	}

	got := selCtx.LeastScoreNodes(-1)
	if got.Len() != 2 {
		t.Fatalf("len(score nodes) = %d, want 2", got.Len())
	}
	if got[0].ID() != "node-b" || got[0].Score != 90 {
		t.Fatalf("highest score = %+v, want node-b=90", got[0])
	}
	if got[1].ID() != "node-a" || got[1].Score != 10 {
		t.Fatalf("lowest score = %+v, want node-a=10", got[1])
	}
}

type testScoreSelector struct {
	weight  float64
	disable bool
	scores  node.NodeScoreList
	err     error
}

func (s testScoreSelector) Select(*selctx.SelectorCtx) (node.NodeScoreList, error) {
	return s.scores, s.err
}

func (s testScoreSelector) ID() string {
	return "test_score"
}

func (s testScoreSelector) Weight() float64 {
	return s.weight
}

func (s testScoreSelector) Disable() bool {
	return s.disable
}
