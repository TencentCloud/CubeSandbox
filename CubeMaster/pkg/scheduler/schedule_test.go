// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package scheduler

import (
	"context"
	"errors"
	"math"
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
			weight: 1,
			err:    errors.New("external scorer unavailable"),
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

func TestRunScoreFilterUsesWeightOncePerScorer(t *testing.T) {
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

	changing := &changingWeightSelector{
		weights: []float64{1, 100, 100}, // first for blend; later values must not be sampled
		scores: node.NodeScoreList{
			{InsID: "node-a", Score: 10, MvmNum: nodeA.MvmNum, OrigNode: nodeA},
			{InsID: "node-b", Score: 90, MvmNum: nodeB.MvmNum, OrigNode: nodeB},
		},
	}
	if err := runScoreFilter(selCtx, []sscore.Selector{changing}); err != nil {
		t.Fatalf("runScoreFilter() error = %v, want nil", err)
	}
	if changing.calls != 1 {
		t.Fatalf("Weight() calls = %d, want 1", changing.calls)
	}
	got := selCtx.LeastScoreNodes(-1)
	if got.Len() != 2 {
		t.Fatalf("len(score nodes) = %d, want 2", got.Len())
	}
	// With a single weight=1 read: scores stay 90 and 10 after / totalPluginWeight.
	if got[0].ID() != "node-b" || got[0].Score != 90 {
		t.Fatalf("highest score = %+v, want node-b=90 (stable weight=1)", got[0])
	}
	if got[1].ID() != "node-a" || got[1].Score != 10 {
		t.Fatalf("lowest score = %+v, want node-a=10 (stable weight=1)", got[1])
	}
}

func TestRunScoreFilterZeroWeightStillSelects(t *testing.T) {
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

	// Zero-weight scorer must still run Select so it can admit nodes (score 0)
	// into the scored set — historical roster-scorer behaviour.
	zero := &countingSelectSelector{
		weight: 0,
		scores: node.NodeScoreList{
			{InsID: "node-b", Score: 99, MvmNum: nodeB.MvmNum, OrigNode: nodeB},
		},
	}
	active := testScoreSelector{
		weight: 1,
		scores: node.NodeScoreList{
			{InsID: "node-a", Score: 50, MvmNum: nodeA.MvmNum, OrigNode: nodeA},
		},
	}
	if err := runScoreFilter(selCtx, []sscore.Selector{zero, active}); err != nil {
		t.Fatalf("runScoreFilter() error = %v, want nil", err)
	}
	if zero.selects != 1 {
		t.Fatalf("zero-weight Select calls = %d, want 1", zero.selects)
	}
	got := selCtx.LeastScoreNodes(-1)
	if got.Len() != 2 {
		t.Fatalf("len(score nodes) = %d, want 2 (zero-weight reshape)", got.Len())
	}
	byID := map[string]float64{}
	for _, n := range got {
		byID[n.ID()] = n.Score
	}
	if byID["node-a"] != 50 {
		t.Fatalf("node-a score = %v, want 50", byID["node-a"])
	}
	if byID["node-b"] != 0 {
		t.Fatalf("node-b score = %v, want 0 from zero-weight admit", byID["node-b"])
	}
}

func TestRunScoreFilterSkipsNonFiniteWeightBlend(t *testing.T) {
	origPostScore := scheduler.postScore
	defer func() {
		scheduler.postScore = origPostScore
	}()
	scheduler.postScore = nil

	nodeA := &node.Node{InsID: "node-a", MvmNum: 1}
	selCtx := selctx.New("random")
	selCtx.Ctx = context.Background()
	selCtx.SetNodes(node.NodeList{nodeA})

	nan := &countingSelectSelector{
		weight: math.NaN(),
		scores: node.NodeScoreList{
			{InsID: "node-a", Score: 99, MvmNum: nodeA.MvmNum, OrigNode: nodeA},
		},
	}
	active := testScoreSelector{
		weight: 1,
		scores: node.NodeScoreList{
			{InsID: "node-a", Score: 50, MvmNum: nodeA.MvmNum, OrigNode: nodeA},
		},
	}
	if err := runScoreFilter(selCtx, []sscore.Selector{nan, active}); err != nil {
		t.Fatalf("runScoreFilter() error = %v, want nil", err)
	}
	// Select still runs so live-config scorers can emit their own observability;
	// the NaN weight must not enter the blend.
	if nan.selects != 1 {
		t.Fatalf("NaN-weight Select calls = %d, want 1", nan.selects)
	}
	got := selCtx.LeastScoreNodes(-1)
	if got.Len() != 1 || got[0].Score != 50 {
		t.Fatalf("scores = %+v, want node-a=50 from active scorer only", got)
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

// changingWeightSelector returns a different weight on each Weight() call to
// detect runScoreFilter re-reading mid-blend.
type changingWeightSelector struct {
	weights []float64
	calls   int
	scores  node.NodeScoreList
}

func (s *changingWeightSelector) Select(*selctx.SelectorCtx) (node.NodeScoreList, error) {
	return s.scores, nil
}

func (s *changingWeightSelector) ID() string { return "changing_weight_score" }

func (s *changingWeightSelector) Weight() float64 {
	if s.calls >= len(s.weights) {
		return s.weights[len(s.weights)-1]
	}
	w := s.weights[s.calls]
	s.calls++
	return w
}

func (s *changingWeightSelector) Disable() bool { return false }

type countingSelectSelector struct {
	weight  float64
	selects int
	scores  node.NodeScoreList
}

func (s *countingSelectSelector) Select(*selctx.SelectorCtx) (node.NodeScoreList, error) {
	s.selects++
	if s.scores != nil {
		return s.scores, nil
	}
	return node.NodeScoreList{{InsID: "node-a", Score: 1}}, nil
}
func (s *countingSelectSelector) ID() string      { return "counting_select" }
func (s *countingSelectSelector) Weight() float64 { return s.weight }
func (s *countingSelectSelector) Disable() bool   { return false }
