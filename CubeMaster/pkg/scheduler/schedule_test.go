// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package scheduler

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
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

func TestRunScoreFilterAbortsOnFailClosedError(t *testing.T) {
	origPostScore := scheduler.postScore
	defer func() {
		scheduler.postScore = origPostScore
	}()
	scheduler.postScore = nil

	nodeA := &node.Node{InsID: "node-a", MvmNum: 1}
	selCtx := selctx.New("random")
	selCtx.Ctx = context.Background()
	selCtx.SetNodes(node.NodeList{nodeA})

	closed := &sscore.FailClosedError{Err: errors.New("sidecar down")}
	err := runScoreFilter(selCtx, []sscore.Selector{
		testScoreSelector{err: closed},
		testScoreSelector{
			weight: 1,
			scores: node.NodeScoreList{
				{InsID: "node-a", Score: 50, MvmNum: nodeA.MvmNum, OrigNode: nodeA},
			},
		},
	})
	if !sscore.IsFailClosed(err) {
		t.Fatalf("runScoreFilter() error = %v, want FailClosedError", err)
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
	resetScoreNonFiniteWeightWarnStateForTest()
	t.Cleanup(resetScoreNonFiniteWeightWarnStateForTest)

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
	if scoreNonFiniteWeightWarnCount.Load() != 1 {
		t.Fatalf("non-finite weight warns = %d, want 1", scoreNonFiniteWeightWarnCount.Load())
	}
	got := selCtx.LeastScoreNodes(-1)
	if got.Len() != 1 || got[0].Score != 50 {
		t.Fatalf("scores = %+v, want node-a=50 from active scorer only", got)
	}
}

func TestRunScoreFilterSkipsNonFiniteNodeScoreBlend(t *testing.T) {
	origPostScore := scheduler.postScore
	defer func() {
		scheduler.postScore = origPostScore
	}()
	scheduler.postScore = nil
	resetScoreNonFiniteWeightWarnStateForTest()
	t.Cleanup(resetScoreNonFiniteWeightWarnStateForTest)

	nodeA := &node.Node{InsID: "node-a", MvmNum: 1}
	selCtx := selctx.New("random")
	selCtx.Ctx = context.Background()
	selCtx.SetNodes(node.NodeList{nodeA})

	poison := testScoreSelector{
		weight: 1,
		scores: node.NodeScoreList{
			{InsID: "node-a", Score: math.NaN(), MvmNum: nodeA.MvmNum, OrigNode: nodeA},
		},
	}
	active := testScoreSelector{
		weight: 1,
		scores: node.NodeScoreList{
			{InsID: "node-a", Score: 50, MvmNum: nodeA.MvmNum, OrigNode: nodeA},
		},
	}
	if err := runScoreFilter(selCtx, []sscore.Selector{poison, active}); err != nil {
		t.Fatalf("runScoreFilter() error = %v, want nil", err)
	}
	if scoreNonFiniteWeightWarnCount.Load() != 1 {
		t.Fatalf("non-finite score warns = %d, want 1", scoreNonFiniteWeightWarnCount.Load())
	}
	got := selCtx.LeastScoreNodes(-1)
	if got.Len() != 1 || got[0].Score != 50 || math.IsNaN(got[0].Score) {
		t.Fatalf("scores = %+v, want finite node-a=50 from active scorer only", got)
	}
}

func TestRunScoreFilterNegativeWeightStillBlends(t *testing.T) {
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

	// Historical built-in behaviour: negative weight still blends (penalty).
	penalty := testScoreSelector{
		weight: -1,
		scores: node.NodeScoreList{
			{InsID: "node-a", Score: 10, MvmNum: nodeA.MvmNum, OrigNode: nodeA},
			{InsID: "node-b", Score: 90, MvmNum: nodeB.MvmNum, OrigNode: nodeB},
		},
	}
	base := testScoreSelector{
		weight: 2,
		scores: node.NodeScoreList{
			{InsID: "node-a", Score: 50, MvmNum: nodeA.MvmNum, OrigNode: nodeA},
			{InsID: "node-b", Score: 50, MvmNum: nodeB.MvmNum, OrigNode: nodeB},
		},
	}
	if err := runScoreFilter(selCtx, []sscore.Selector{penalty, base}); err != nil {
		t.Fatalf("runScoreFilter() error = %v, want nil", err)
	}
	got := selCtx.LeastScoreNodes(-1)
	if got.Len() != 2 {
		t.Fatalf("len = %d, want 2", got.Len())
	}
	// totalPluginWeight = -1+2 = 1
	// node-a: (10*-1 + 50*2)/1 = 90; node-b: (90*-1 + 50*2)/1 = 10
	if got[0].ID() != "node-a" || got[0].Score != 90 {
		t.Fatalf("highest = %+v, want node-a=90", got[0])
	}
	if got[1].ID() != "node-b" || got[1].Score != 10 {
		t.Fatalf("lowest = %+v, want node-b=10", got[1])
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

type passThroughPreFilter struct{}

func (passThroughPreFilter) Select(selCtx *selctx.SelectorCtx) (node.NodeList, error) {
	return selCtx.Nodes(), nil
}

func (passThroughPreFilter) ID() string {
	return "passthrough_prefilter"
}

func TestSchedulerSelectFailOpenContinues(t *testing.T) {
	restoreSchedulerPlugins(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	initSchedulerExternalHTTPScoreFailurePolicy(t, server.URL, "fail_open")

	scheduler.preSelector = passThroughPreFilter{}
	scheduler.filter = nil
	scheduler.score = []sscore.Selector{sscore.NewExternalHTTPScore()}
	scheduler.postScore = nil

	selCtx := schedulerSelectTestCtx()
	got, err := Select(selCtx)
	if err != nil {
		t.Fatalf("fail_open scheduler.Select() error = %v, want nil", err)
	}
	if got == nil {
		t.Fatal("fail_open scheduler.Select() node = nil, want a candidate")
	}
}

func TestSchedulerSelectFailClosedReturnsError(t *testing.T) {
	restoreSchedulerPlugins(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	initSchedulerExternalHTTPScoreFailurePolicy(t, server.URL, "fail_closed")

	scheduler.preSelector = passThroughPreFilter{}
	scheduler.filter = nil
	scheduler.score = []sscore.Selector{sscore.NewExternalHTTPScore()}
	scheduler.postScore = nil

	selCtx := schedulerSelectTestCtx()
	_, err := Select(selCtx)
	if !sscore.IsFailClosed(err) {
		t.Fatalf("fail_closed scheduler.Select() error = %v, want FailClosedError", err)
	}
}

func restoreSchedulerPlugins(t *testing.T) {
	t.Helper()
	origPre := scheduler.preSelector
	origFilter := scheduler.filter
	origScore := scheduler.score
	origPost := scheduler.postScore
	t.Cleanup(func() {
		scheduler.preSelector = origPre
		scheduler.filter = origFilter
		scheduler.score = origScore
		scheduler.postScore = origPost
	})
}

func schedulerSelectTestCtx() *selctx.SelectorCtx {
	ctx := selctx.New("random")
	ctx.Ctx = context.Background()
	ctx.InstanceType = "cubebox"
	ctx.SetNodes(node.NodeList{
		{InsID: "node-a", IP: "10.0.0.1", InstanceType: "cubebox", MvmNum: 1},
		{InsID: "node-b", IP: "10.0.0.2", InstanceType: "cubebox", MvmNum: 2},
	})
	return ctx
}

func initSchedulerExternalHTTPScoreFailurePolicy(t *testing.T, endpoint, policy string) {
	t.Helper()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "cubemaster.yaml")
	content := `common: {}
log: {}
scheduler:
  priority_select_num: 2
  score:
    enable_scorers:
      - external_http_score
    resource_weights:
      mvm_num: 1
    plugin_conf:
      external_http_score:
        weight: 1
        endpoint: "` + endpoint + `"
        timeout: 1s
        failure_policy: ` + policy + `
`
	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	neutralPath := filepath.Join(dir, "neutral.yaml")
	neutral := `common: {}
log: {}
scheduler:
  priority_select_num: -1
`
	if err := os.WriteFile(neutralPath, []byte(neutral), 0644); err != nil {
		t.Fatalf("write neutral config: %v", err)
	}
	t.Setenv("CUBE_MASTER_CONFIG_PATH", configPath)
	// config.Init() replaces the process-global cfg and starts a hotswap
	// watcher that is not closed here. Restore a neutral cfg in Cleanup so
	// later same-package tests do not inherit priority_select_num / scorer
	// roster from these failure_policy cases (another watcher still leaks).
	t.Cleanup(func() {
		if err := os.Setenv("CUBE_MASTER_CONFIG_PATH", neutralPath); err != nil {
			t.Errorf("restore CUBE_MASTER_CONFIG_PATH: %v", err)
			return
		}
		if _, err := config.Init(); err != nil {
			t.Errorf("restore config.Init(): %v", err)
		}
	})
	if _, err := config.Init(); err != nil {
		t.Fatalf("config.Init(): %v", err)
	}
}
