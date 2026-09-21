// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package profile

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/plugin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/plugin/expr"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/score"
)

type trackingCloser struct{ closed bool }

// stubScore is a minimal score.Selector with a configurable weight.
type stubScore struct {
	id     string
	weight float64
}

func (s *stubScore) Select(*selctx.SelectorCtx) (node.NodeScoreList, error) { return nil, nil }
func (s *stubScore) ID() string                                             { return s.id }
func (s *stubScore) Weight() float64                                        { return s.weight }
func (s *stubScore) Disable() bool                                          { return false }

func (c *trackingCloser) Close() error {
	c.closed = true
	return nil
}

var _ io.Closer = (*trackingCloser)(nil)

func profileRegistry(t *testing.T) *plugin.Registry {
	t.Helper()
	registry := plugin.NewRegistry()
	if err := plugin.RegisterBuiltins(registry); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterFilterProvider(plugin.TypeExpression, expr.NewFilter); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterScoreProvider(plugin.TypeExpression, expr.NewScore); err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestProfileRoutingAndLegacyFallback(t *testing.T) {
	cfg := &config.Config{Scheduler: &config.WrapperSchedulerConf{SchedulerConf: config.SchedulerConf{
		PrioritySelectNum:     3,
		ProfileRouteLabelKeys: []string{"workload"},
		Profiles: []config.SchedulerProfileConf{{
			Name:      "burst",
			Route:     config.SchedulerProfileRouteConf{InstanceTypes: []string{"S.*"}, Labels: map[string]string{"workload": "burst"}},
			Scores:    []config.SchedulerProfilePluginConf{{Name: "idle", Type: "expr", Expr: "100.0 - node.cpu_util", Weight: 2}},
			Selection: config.SchedulerSelectionConf{TopN: 5, Method: "spread"},
		}},
	}}}
	set, err := Compile(context.Background(), cfg, profileRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = set.Close() })

	matched := set.Match(&selctx.SelectorCtx{InstanceType: "S2", RequestLabels: map[string]string{"workload": "burst"}})
	if matched.Name != "burst" || matched.TopN != 5 || len(matched.Guards) != len(mandatoryGuardNames) {
		t.Fatalf("matched pipeline = %+v", matched)
	}
	fallback := set.Match(&selctx.SelectorCtx{InstanceType: "L1"})
	if fallback.Name != "default" || fallback.TopN != 3 || len(fallback.Guards) != 0 {
		t.Fatalf("fallback pipeline = %+v", fallback)
	}
}

func TestProfileValidationRejectsUncontrolledLabelsAndUnknownPlugins(t *testing.T) {
	tests := []struct {
		name    string
		profile config.SchedulerProfileConf
	}{
		{
			name: "uncontrolled label",
			profile: config.SchedulerProfileConf{
				Name: "bad", Route: config.SchedulerProfileRouteConf{Labels: map[string]string{"tenant": "x"}},
			},
		},
		{
			name: "unknown plugin",
			profile: config.SchedulerProfileConf{
				Name: "bad", Route: config.SchedulerProfileRouteConf{InstanceTypes: []string{".*"}},
				Filters: []config.SchedulerProfilePluginConf{{Name: "missing"}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{Scheduler: &config.WrapperSchedulerConf{SchedulerConf: config.SchedulerConf{Profiles: []config.SchedulerProfileConf{test.profile}}}}
			if _, err := Compile(context.Background(), cfg, profileRegistry(t)); err == nil {
				t.Fatal("invalid profile must be rejected")
			}
		})
	}
}

func TestCustomDefaultDoesNotCompileUnusedLegacyPlugins(t *testing.T) {
	cfg := &config.Config{Scheduler: &config.WrapperSchedulerConf{SchedulerConf: config.SchedulerConf{
		Filter: &config.SchedulerFilterConf{EnableFilters: []string{"removed-legacy-plugin"}},
		Profiles: []config.SchedulerProfileConf{{
			Name: "custom-default", Default: true,
		}},
	}}}
	set, err := Compile(context.Background(), cfg, profileRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = set.Close() })
	if got := set.Match(&selctx.SelectorCtx{}); got.Name != "custom-default" {
		t.Fatalf("default profile = %q", got.Name)
	}
}

func TestLegacyCompileSkipsUnknownPluginsAndZeroWeights(t *testing.T) {
	registry := profileRegistry(t)
	zeroWeight := &stubScore{id: "zero", weight: 0}
	if err := registry.RegisterScore(plugin.TypeGo, "zero_weight_scorer", func(context.Context, config.SchedulerProfilePluginConf) (score.Selector, error) {
		return zeroWeight, nil
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Scheduler: &config.WrapperSchedulerConf{SchedulerConf: config.SchedulerConf{
		PrioritySelectNum: 1,
		Filter:            &config.SchedulerFilterConf{EnableFilters: []string{"removed-plugin", "cpu"}},
		Score: &config.SchedulerScoreConf{
			EnableScorers:   []string{"removed-scorer", "zero_weight_scorer"},
			ResourceWeights: map[string]float64{},
		},
	}}}
	set, err := Compile(context.Background(), cfg, registry)
	if err != nil {
		t.Fatalf("legacy compile must tolerate stale plugin names and zero weights: %v", err)
	}
	t.Cleanup(func() { _ = set.Close() })
	pipeline := set.Match(&selctx.SelectorCtx{})
	if len(pipeline.Filters) != 1 || pipeline.Filters[0].Name != "cpu" {
		t.Fatalf("legacy filters = %+v", pipeline.Filters)
	}
	if len(pipeline.Scores) != 0 {
		t.Fatalf("zero-weight scorer must be skipped, scores = %+v", pipeline.Scores)
	}
}

func TestProfileSetDefersCloseUntilLeaseRelease(t *testing.T) {
	closer := &trackingCloser{}
	set := &Set{fallback: &Pipeline{Name: "default"}, closers: []io.Closer{closer}}
	pipeline, release, ok := set.Acquire(&selctx.SelectorCtx{})
	if !ok || pipeline == nil || pipeline.Name != "default" {
		t.Fatalf("acquire = (%+v, %v)", pipeline, ok)
	}
	if err := set.Close(); err != nil {
		t.Fatal(err)
	}
	if closer.closed {
		t.Fatal("active lease must keep plugin connection open")
	}
	if _, _, ok := set.Acquire(&selctx.SelectorCtx{}); ok {
		t.Fatal("retired profile set accepted a new lease")
	}
	release()
	if !closer.closed {
		t.Fatal("last lease release must close plugin connection")
	}
}

// TestProfileCompileRequiresLegacyConfForCoupledScorers pins the compile-time
// validation for built-in scorers that still read their factor switches from
// the legacy scheduler.score tree: a profile referencing them without the
// legacy block must fail to compile instead of silently scoring nothing.
func TestProfileCompileRequiresLegacyConfForCoupledScorers(t *testing.T) {
	profileConf := config.SchedulerProfileConf{
		Name:    "reuse",
		Default: true,
		Scores: []config.SchedulerProfilePluginConf{
			{Name: "image_score", Type: "go", Weight: 1},
		},
	}

	// Without the legacy plugin_conf block: compile must fail.
	cfg := &config.Config{Scheduler: &config.WrapperSchedulerConf{SchedulerConf: config.SchedulerConf{
		Profiles: []config.SchedulerProfileConf{profileConf},
	}}}
	if _, err := Compile(context.Background(), cfg, profileRegistry(t)); err == nil {
		t.Fatal("profile referencing image_score without scheduler.score.plugin_conf.image_score must be rejected")
	} else if !strings.Contains(err.Error(), "plugin_conf.image_score") {
		t.Fatalf("error must point at the missing legacy block, got: %v", err)
	}

	// With the legacy block present: compile succeeds.
	cfg.Scheduler.Score = &config.SchedulerScoreConf{
		ResourceWeights: map[string]float64{"template_id": 1},
		ScorePluginConf: config.ScorePluginConf{ImageScore: &config.ImageScore{
			Weight:              1,
			EnableWeightFactors: []string{"template_id"},
		}},
	}
	set, err := Compile(context.Background(), cfg, profileRegistry(t))
	if err != nil {
		t.Fatalf("compile with the legacy block present: %v", err)
	}
	t.Cleanup(func() { _ = set.Close() })
}

func TestProfileCompileRejectsDegenerateLegacyCoupledScorers(t *testing.T) {
	profileFor := func(name string) config.SchedulerProfileConf {
		return config.SchedulerProfileConf{
			Name:    "p",
			Default: true,
			Scores:  []config.SchedulerProfilePluginConf{{Name: name, Type: "go", Weight: 1}},
		}
	}
	tests := []struct {
		name    string
		scorer  string
		score   *config.SchedulerScoreConf
		wantErr string
	}{
		{
			name:   "image_score without resource_weights",
			scorer: "image_score",
			score: &config.SchedulerScoreConf{
				ScorePluginConf: config.ScorePluginConf{ImageScore: &config.ImageScore{
					Weight: 1, EnableWeightFactors: []string{"image_id"}}},
			},
			wantErr: "resource_weights",
		},
		{
			name:   "image_score with zero-sum factors",
			scorer: "image_score",
			score: &config.SchedulerScoreConf{
				ResourceWeights: map[string]float64{"image_id": 0},
				ScorePluginConf: config.ScorePluginConf{ImageScore: &config.ImageScore{
					Weight: 1, EnableWeightFactors: []string{"image_id"}}},
			},
			wantErr: "no enabled factor",
		},
		{
			name:   "image_score disabled in legacy conf",
			scorer: "image_score",
			score: &config.SchedulerScoreConf{
				ResourceWeights: map[string]float64{"image_id": 1},
				ScorePluginConf: config.ScorePluginConf{ImageScore: &config.ImageScore{
					Weight: 1, EnableWeightFactors: []string{"image_id"}, Disable: true}},
			},
			wantErr: "disable",
		},
		{
			name:   "multi_factor_weighted_average without resource_weights",
			scorer: "multi_factor_weighted_average",
			score: &config.SchedulerScoreConf{
				ScorePluginConf: config.ScorePluginConf{MultiFactorWeightedAverage: &config.MultiFactorWeightedAverage{
					Weight: 1, EnableWeightFactors: []string{"mvm_num"}}},
			},
			wantErr: "resource_weights",
		},
		{
			name:   "real_time_weighted_average with zero-sum factors",
			scorer: "real_time_weighted_average",
			score: &config.SchedulerScoreConf{
				ResourceWeights: map[string]float64{},
				ScorePluginConf: config.ScorePluginConf{RealTimeWeightedAverage: &config.RealTimeWeightedAverage{
					Weight: 1, EnableWeightFactors: []string{"mvm_num"}}},
			},
			wantErr: "no enabled factor",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{Scheduler: &config.WrapperSchedulerConf{SchedulerConf: config.SchedulerConf{
				Score:    test.score,
				Profiles: []config.SchedulerProfileConf{profileFor(test.scorer)},
			}}}
			if _, err := Compile(context.Background(), cfg, profileRegistry(t)); err == nil {
				t.Fatalf("degenerate legacy conf for %q must be rejected", test.scorer)
			} else if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error must mention %q, got: %v", test.wantErr, err)
			}
		})
	}
}
