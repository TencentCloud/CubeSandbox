// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/profile"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
	sfilter "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/filter"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/plugin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/score"
)

// compatScore is a deterministic score.Selector with a configurable weight,
// used to pin scoring parity between the legacy and profile compile paths.
type compatScore struct {
	id     string
	weight float64
	values map[string]float64
}

func (s compatScore) ID() string      { return s.id }
func (s compatScore) Weight() float64 { return s.weight }
func (s compatScore) Disable() bool   { return false }
func (s compatScore) Select(selection *selctx.SelectorCtx) (node.NodeScoreList, error) {
	result := make(node.NodeScoreList, 0, selection.Nodes().Len())
	for _, candidate := range selection.Nodes() {
		result = append(result, &node.NodeScore{InsID: candidate.ID(), OrigNode: candidate, Score: s.values[candidate.ID()]})
	}
	return result, nil
}

// compatRegistry registers the real built-ins (the explicit profile compiles
// the mandatory guards from them) plus deterministic stub plugins shared by
// both compile paths under test.
func compatRegistry(t *testing.T) *plugin.Registry {
	t.Helper()
	registry := plugin.NewRegistry()
	if err := plugin.RegisterBuiltins(registry); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterFilter(plugin.TypeGo, "drop_n4", func(context.Context, config.SchedulerProfilePluginConf) (sfilter.Selector, error) {
		return executorFilter{id: "drop_n4", keep: map[string]bool{"n1": true, "n2": true, "n3": true}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	stubScorers := []compatScore{
		{id: "compat_hi", weight: 2, values: map[string]float64{"n1": 90, "n2": 60, "n3": 30}},
		{id: "compat_lo", weight: 1, values: map[string]float64{"n1": 10, "n2": 80, "n3": 50}},
	}
	for _, scorer := range stubScorers {
		scorer := scorer
		if err := registry.RegisterScore(plugin.TypeGo, scorer.id, func(context.Context, config.SchedulerProfilePluginConf) (score.Selector, error) {
			return scorer, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	return registry
}

// initCompatConfig loads a minimal global config so the mandatory built-in
// guards (node_safety/cpu/mem/disk read thresholds from config.GetConfig)
// can execute inside this package.
func initCompatConfig(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	confPath := filepath.Join(dir, "conf.yaml")
	content := []byte("common: {}\nlog:\n  module: profile-compat-test\n  path: " + filepath.Join(dir, "log") + "\n")
	if err := os.WriteFile(confPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CUBE_MASTER_CONFIG_PATH", confPath)
	if _, err := config.Init(); err != nil {
		t.Fatalf("config init for built-in guards: %v", err)
	}
}

// compatNodes returns four healthy nodes that pass every mandatory guard:
// fresh metrics, quota/load headroom for a 1C1G request, disk usage below the
// threshold and no create-concurrency pressure.
func compatNodes() node.NodeList {
	now := time.Now()
	nodes := node.NodeList{}
	for i, id := range []string{"n1", "n2", "n3", "n4"} {
		nodes = append(nodes, &node.Node{
			Index: i + 1, InsID: id, IP: "10.0.0." + id[1:],
			Healthy: true, MvmNum: int64(i * 3),
			CpuTotal: 64000, CpuLoadUsage: float64(1000 * (i + 1)),
			QuotaCpu: 64000, QuotaCpuUsage: int64(8000 * (i + 1)),
			CpuUtil:  float64(10 * (i + 1)),
			QuotaMem: 131072, QuotaMemUsage: int64(8192 * (i + 1)),
			MemMBTotal: 131072, MemUsage: int64(8192 * (i + 1)),
			StorageDiskUsagePer: 10, SysDiskUsagePer: 10, DataDiskUsagePer: 10,
			MetricUpdate: now, MetricLocalUpdateAt: now,
		})
	}
	return nodes
}

// runCompatPipeline drives one compiled pipeline through the same stages as
// Select (snapshot freeze -> guards -> filters -> scores -> selection) over a
// fresh 1C1G request and the shared node fixture.
func runCompatPipeline(t *testing.T, pipeline *profile.Pipeline) (*selctx.SelectorCtx, *node.Node) {
	t.Helper()
	selCtx := selctx.New("random")
	selCtx.Ctx = context.Background()
	selCtx.ReqRes = &selctx.RequestResource{
		Cpu: resource.MustParse("1"), Mem: resource.MustParse("1Gi"),
	}
	selCtx.SetProfileName(pipeline.Name)
	selCtx.SetNodes(compatNodes())
	freezeSnapshot(selCtx)
	if err := runProfileFilters(selCtx, pluginKindGuard, pipeline.Guards); err != nil {
		t.Fatalf("profile %q guards: %v", pipeline.Name, err)
	}
	if err := runProfileFilters(selCtx, pluginKindFilter, pipeline.Filters); err != nil {
		t.Fatalf("profile %q filters: %v", pipeline.Name, err)
	}
	if err := runProfileScores(selCtx, pipeline.Scores); err != nil {
		t.Fatalf("profile %q scores: %v", pipeline.Name, err)
	}
	return selCtx, selectNode(selCtx, pipeline)
}

// TestLegacyAndExplicitProfileSelectIdentically is the same-input selection
// anchor for the "default profile = compatibility anchor" contract: a legacy
// static filter/score configuration and a semantically equivalent explicit
// default profile must produce identical scheduling outcomes over identical
// node state and the same request.
//
// Equivalence setup: both sides run the same stub filter (drop n4) and the
// same two stub scorers; scorer weights come from selector.Weight() on both
// paths (legacy always, explicit because plugin weight is left at 0), and
// both use weighted-random selection with top_n=1, which is deterministic.
//
// Known residual differences, by construction unobservable on this input:
//   - the explicit profile always prepends the six mandatory guards
//     (profile.mandatoryGuardNames) while the legacy pipeline has none; the
//     healthy fixture nodes pass all of them, so the guard stage is a
//     pass-through here (in production the prefilter applies the same checks);
//   - the score failure policy differs (legacy skip-on-error vs profile
//     default-score/fail-closed) and the profile path force-enables scorers
//     ([0,100] range and full candidate coverage checks); both are inert
//     because the stub scorers never fail and return valid full coverage;
//   - with top_n>1 the final pick is seeded by time and cannot be asserted
//     across runs, so per-node final scores and ordering are the primary
//     assertions and the selected node is asserted at top_n=1.
func TestLegacyAndExplicitProfileSelectIdentically(t *testing.T) {
	initCompatConfig(t)
	registry := compatRegistry(t)
	ctx := context.Background()

	legacyCfg := &config.Config{Scheduler: &config.WrapperSchedulerConf{SchedulerConf: config.SchedulerConf{
		PrioritySelectNum: 1,
		Filter:            &config.SchedulerFilterConf{EnableFilters: []string{"drop_n4"}},
		Score: &config.SchedulerScoreConf{
			EnableScorers:   []string{"compat_hi", "compat_lo"},
			ResourceWeights: map[string]float64{},
		},
	}}}
	legacySet, err := profile.Compile(ctx, legacyCfg, registry)
	if err != nil {
		t.Fatalf("compile legacy pipeline: %v", err)
	}
	t.Cleanup(func() { _ = legacySet.Close() })
	legacyPipeline := legacySet.Match(&selctx.SelectorCtx{})
	if legacyPipeline == nil || len(legacyPipeline.Guards) != 0 {
		t.Fatalf("legacy pipeline = %+v, want guard-less fallback", legacyPipeline)
	}

	explicitCfg := &config.Config{Scheduler: &config.WrapperSchedulerConf{SchedulerConf: config.SchedulerConf{
		Profiles: []config.SchedulerProfileConf{{
			Name:    "legacy_mirror",
			Default: true,
			Filters: []config.SchedulerProfilePluginConf{{Name: "drop_n4"}},
			Scores:  []config.SchedulerProfilePluginConf{{Name: "compat_hi"}, {Name: "compat_lo"}},
			Selection: config.SchedulerSelectionConf{
				TopN: 1, Method: profile.SelectionRandom,
			},
			Failure: config.SchedulerFailureConf{NoCandidate: string(profile.NoCandidateBackoff)},
		}},
	}}}
	explicitSet, err := profile.Compile(ctx, explicitCfg, registry)
	if err != nil {
		t.Fatalf("compile explicit profile: %v", err)
	}
	t.Cleanup(func() { _ = explicitSet.Close() })
	explicitPipeline := explicitSet.Match(&selctx.SelectorCtx{})
	if explicitPipeline == nil || len(explicitPipeline.Guards) != 6 {
		t.Fatalf("explicit pipeline = %+v, want the six mandatory guards", explicitPipeline)
	}

	legacyCtx, legacyPick := runCompatPipeline(t, legacyPipeline)
	explicitCtx, explicitPick := runCompatPipeline(t, explicitPipeline)

	legacyScores := legacyCtx.LeastScoreNodes(-1)
	explicitScores := explicitCtx.LeastScoreNodes(-1)
	if legacyScores.Len() != explicitScores.Len() {
		t.Fatalf("score count differs: legacy=%d explicit=%d", legacyScores.Len(), explicitScores.Len())
	}
	// n4 is filtered out; aggregated scores are (hi*2 + lo*1)/3:
	// n2 = 200/3 > n1 = 190/3 > n3 = 110/3.
	wantOrder := []string{"n2", "n1", "n3"}
	wantScores := []float64{200.0 / 3, 190.0 / 3, 110.0 / 3}
	for i := range legacyScores {
		if legacyScores[i].ID() != explicitScores[i].ID() {
			t.Fatalf("score order differs at %d: legacy=%s explicit=%s", i, legacyScores[i].ID(), explicitScores[i].ID())
		}
		if legacyScores[i].Score != explicitScores[i].Score {
			t.Fatalf("score differs for %s: legacy=%v explicit=%v", legacyScores[i].ID(), legacyScores[i].Score, explicitScores[i].Score)
		}
		if legacyScores[i].ID() != wantOrder[i] || legacyScores[i].Score != wantScores[i] {
			t.Fatalf("unexpected score at %d: got (%s, %v), want (%s, %v)",
				i, legacyScores[i].ID(), legacyScores[i].Score, wantOrder[i], wantScores[i])
		}
	}
	if legacyPick == nil || explicitPick == nil {
		t.Fatalf("selection failed: legacy=%v explicit=%v", legacyPick, explicitPick)
	}
	if legacyPick.ID() != explicitPick.ID() {
		t.Fatalf("selected node differs: legacy=%s explicit=%s", legacyPick.ID(), explicitPick.ID())
	}
	if legacyPick.ID() != "n2" {
		t.Fatalf("top_n=1 must select the highest-scored node n2, got %s", legacyPick.ID())
	}
}
