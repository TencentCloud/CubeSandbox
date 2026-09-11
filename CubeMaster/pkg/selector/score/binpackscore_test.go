// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package score

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

func TestBinpackBuiltinProfileInjectsDefaults(t *testing.T) {
	if runIsolatedScoreConfigTest(t) {
		return
	}
	initBinpackScoreTestConfig(t, `common: {}
log: {}
scheduler:
  profile: binpack_utilization
`)

	scorer := NewBinpackScore()
	if scorer.Disable() {
		t.Fatal("built-in binpack profile should inject an enabled config")
	}
	if scorer.Weight() != 1 {
		t.Fatalf("Weight() = %v, want 1", scorer.Weight())
	}
	if scorer.ID() != constants.SelectorScoreID+"/"+binpackScoreName {
		t.Fatalf("ID() = %s, want binpack_score", scorer.ID())
	}
}

func TestBinpackBuiltinProfilePreservesExplicitConfig(t *testing.T) {
	if runIsolatedScoreConfigTest(t) {
		return
	}
	tests := []struct {
		name        string
		pluginYAML  string
		wantWeight  float64
		wantDisable bool
		wantInitErr string
	}{
		{
			name:       "custom weight",
			pluginYAML: "        weight: 3\n        cpu_weight: 2\n",
			wantWeight: 3,
		},
		{
			name:        "zero weight conflicts with builtin profile",
			pluginYAML:  "        weight: 0\n",
			wantInitErr: "explicitly disabled",
		},
		{
			name:        "explicit disable conflicts with builtin profile",
			pluginYAML:  "        weight: 3\n        disable: true\n",
			wantInitErr: "explicitly disabled",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cubemaster.yaml")
			content := `common: {}
log: {}
scheduler:
  profile: binpack_utilization
  score:
    plugin_conf:
      binpack_score:
` + tt.pluginYAML
			if err := os.WriteFile(path, []byte(content), 0644); err != nil {
				t.Fatalf("write config: %v", err)
			}
			t.Setenv("CUBE_MASTER_CONFIG_PATH", path)
			_, err := config.Init()
			if tt.wantInitErr != "" {
				if err == nil {
					t.Fatal("config.Init() error = nil, want conflict fail-fast")
				}
				if !strings.Contains(err.Error(), tt.wantInitErr) {
					t.Fatalf("config.Init() error = %v, want substring %q", err, tt.wantInitErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("config.Init(): %v", err)
			}

			cfg := config.GetConfig().Scheduler.Score.ScorePluginConf.BinpackScore
			if cfg == nil {
				t.Fatal("plugin_conf.binpack_score = nil, want explicit config preserved")
			}
			w, disabled := config.BinpackPluginWeight(cfg)
			if w != tt.wantWeight {
				t.Fatalf("effective weight = %v, want %v", w, tt.wantWeight)
			}
			if disabled != tt.wantDisable {
				t.Fatalf("BinpackPluginWeight disabled = %v, want %v", disabled, tt.wantDisable)
			}
			if tt.name == "custom weight" && cfg.CPUWeight != 2 {
				t.Fatalf("config cpu_weight = %v, want 2", cfg.CPUWeight)
			}

			scorer := NewBinpackScore()
			if scorer.Disable() != tt.wantDisable {
				t.Fatalf("Disable() = %v, want %v", scorer.Disable(), tt.wantDisable)
			}
			if !tt.wantDisable && scorer.Weight() != tt.wantWeight {
				t.Fatalf("Weight() = %v, want %v", scorer.Weight(), tt.wantWeight)
			}
		})
	}
}

func TestBinpackScoreDisableSkipsSelect(t *testing.T) {
	if runIsolatedScoreConfigTest(t) {
		return
	}
	initBinpackScoreTestConfig(t, `common: {}
log: {}
scheduler:
  score:
    enable_scorers:
      - binpack_score
    plugin_conf:
      binpack_score:
        disable: true
`)

	scorer := NewBinpackScore()
	if !scorer.Disable() {
		t.Fatal("Disable() = false, want true")
	}
	got, err := scorer.Select(binpackScoreTestCtx(t))
	if err != nil {
		t.Fatalf("Select() error = %v, want nil", err)
	}
	if got != nil {
		t.Fatalf("Select() = %+v, want nil", got)
	}
}

func TestBinpackScoreZeroWeightDisablesAndSkipsSelect(t *testing.T) {
	if runIsolatedScoreConfigTest(t) {
		return
	}
	initBinpackScoreTestConfig(t, `common: {}
log: {}
scheduler:
  score:
    enable_scorers:
      - binpack_score
    plugin_conf:
      binpack_score:
        weight: 0
`)

	scorer := NewBinpackScore()
	if !scorer.Disable() {
		t.Fatal("Disable() = false, want true for weight: 0")
	}
	if scorer.Weight() != 0 {
		t.Fatalf("Weight() = %v, want 0", scorer.Weight())
	}
	got, err := scorer.Select(binpackScoreTestCtx(t))
	if err != nil {
		t.Fatalf("Select() error = %v, want nil", err)
	}
	if got != nil {
		t.Fatalf("Select() = %+v, want nil", got)
	}
}

func TestBinpackScorePrefersFullerNode(t *testing.T) {
	if runIsolatedScoreConfigTest(t) {
		return
	}
	initBinpackScoreTestConfig(t, `common: {}
log: {}
scheduler:
  ignore_redis_allocation: false
  overcommit_ratio:
    cpu_ratio: 1
    mem_ratio: 1
  score:
    enable_scorers:
      - binpack_score
    plugin_conf:
      binpack_score:
        weight: 1
`)

	scorer := NewBinpackScore()
	got, err := scorer.Select(binpackScoreTestCtx(t))
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if got.Len() != 2 {
		t.Fatalf("len(scores) = %d, want 2", got.Len())
	}

	byID := map[string]float64{}
	for i := range got {
		byID[got[i].ID()] = got[i].Score
	}
	if byID["node-full"] <= byID["node-empty"] {
		t.Fatalf("scores = %+v, want node-full > node-empty", byID)
	}
}

func TestBinpackScoreNegativeWeightRejectedAtConfigLoad(t *testing.T) {
	if runIsolatedScoreConfigTest(t) {
		return
	}
	path := filepath.Join(t.TempDir(), "cubemaster.yaml")
	content := `common: {}
log: {}
scheduler:
  score:
    enable_scorers:
      - binpack_score
    plugin_conf:
      binpack_score:
        weight: -2
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("CUBE_MASTER_CONFIG_PATH", path)
	_, err := config.Init()
	if err == nil {
		t.Fatal("config.Init() error = nil, want negative weight rejection")
	}
	if !strings.Contains(err.Error(), "binpack_score.weight must be >= 0") {
		t.Fatalf("config.Init() error = %v, want negative weight rejection", err)
	}
}

func TestBinpackScoreRegisteredByConfig(t *testing.T) {
	if runIsolatedScoreConfigTest(t) {
		return
	}
	initBinpackScoreTestConfig(t, `common: {}
log: {}
scheduler:
  score:
    enable_scorers:
      - binpack_score
    plugin_conf:
      binpack_score:
        weight: 1
`)

	selectors := NewSelector(context.Background())
	if len(selectors) != 1 {
		t.Fatalf("len(selectors) = %d, want 1", len(selectors))
	}
	if selectors[0].ID() != constants.SelectorScoreID+"/"+binpackScoreName {
		t.Fatalf("selector ID = %s, want binpack_score", selectors[0].ID())
	}
}

func TestBinpackOccupancyBoundaryAndMonotonicity(t *testing.T) {
	if runIsolatedScoreConfigTest(t) {
		return
	}
	initBinpackScoreTestConfig(t, `common: {}
log: {}
scheduler:
  ignore_redis_allocation: false
  node_max_mvm_num: 100
  score:
    enable_scorers:
      - binpack_score
    plugin_conf:
      binpack_score:
        weight: 1
        cpu_weight: 1
        mem_weight: 1
        mvm_weight: 1
`)

	base := func(cpuUsed, memUsed, mvmUsed, maxMvm int64) *node.Node {
		return &node.Node{
			InsID:         "n",
			QuotaCpu:      1000,
			QuotaMem:      1000,
			QuotaCpuUsage: cpuUsed,
			QuotaMemUsage: memUsed,
			MvmNum:        mvmUsed,
			MaxMvmLimit:   maxMvm,
		}
	}

	cases := []struct {
		name string
		node *node.Node
		// relative checks vs empty / fuller siblings are below; here we pin
		// bounded range and known ratios for fixed fixtures.
		wantMin float64
		wantMax float64
	}{
		{name: "empty", node: base(0, 0, 0, 10), wantMin: 0, wantMax: 0},
		{name: "partial", node: base(250, 250, 2, 10), wantMin: 20, wantMax: 30},
		{name: "nearly_full", node: base(900, 900, 9, 10), wantMin: 89, wantMax: 91},
		{name: "full", node: base(1000, 1000, 10, 10), wantMin: 100, wantMax: 100},
		{name: "over_reported_clamped", node: base(2000, 2000, 20, 10), wantMin: 100, wantMax: 100},
		{name: "zero_quota_cpu_mem_degrades", node: &node.Node{InsID: "z", QuotaCpu: 0, QuotaMem: 0, QuotaCpuUsage: 5, QuotaMemUsage: 5, MvmNum: 5, MaxMvmLimit: 10}, wantMin: 0, wantMax: 100},
		{name: "missing_max_mvm_uses_authoritative_fallback", node: base(0, 0, 50, 0), wantMin: 0, wantMax: 100},
	}
	prev := -1.0
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := binpackOccupancyScore(tc.node, 1, 1, 1)
			if got < tc.wantMin || got > tc.wantMax {
				t.Fatalf("score = %v, want in [%v, %v]", got, tc.wantMin, tc.wantMax)
			}
			if got < 0 || got > 100 {
				t.Fatalf("score = %v outside documented [0,100] range", got)
			}
		})
	}

	// Monotonicity on a single axis: increasing CPU occupancy never lowers score.
	for _, cpu := range []int64{0, 100, 400, 700, 1000, 1500} {
		got := binpackOccupancyScore(base(cpu, 0, 0, 10), 1, 0, 0)
		if got < prev {
			t.Fatalf("cpu occupancy not monotonic: cpu=%d score=%v prev=%v", cpu, got, prev)
		}
		prev = got
	}
}

func TestBinpackUsesAuthoritativeMaxMvmWhenNodeLimitMissing(t *testing.T) {
	if runIsolatedScoreConfigTest(t) {
		return
	}
	initBinpackScoreTestConfig(t, `common: {}
log: {}
scheduler:
  node_max_mvm_num: 100
  score:
    enable_scorers:
      - binpack_score
    plugin_conf:
      binpack_score:
        weight: 1
        cpu_weight: 0
        mem_weight: 0
        mvm_weight: 1
`)

	// cpu/mem factor weights of 0 fall back to default 1 in runtime; force
	// MVM-only by calling occupancy with explicit weights.
	n := &node.Node{InsID: "n", QuotaCpu: 1000, QuotaMem: 1000, MvmNum: 50, MaxMvmLimit: 0}
	got := binpackOccupancyScore(n, 0, 0, 1)
	// MaxMvmLimit(n) falls back to node_max_mvm_num=100, so 50/100 => 50.
	if got != 50 {
		t.Fatalf("score = %v, want 50 via authoritative MaxMvmLimit fallback", got)
	}
}

func initBinpackScoreTestConfig(t *testing.T, yamlBody string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cubemaster.yaml")
	if err := os.WriteFile(path, []byte(yamlBody), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("CUBE_MASTER_CONFIG_PATH", path)
	if _, err := config.Init(); err != nil {
		t.Fatalf("config.Init(): %v", err)
	}
}

func binpackScoreTestCtx(t *testing.T) *selctx.SelectorCtx {
	t.Helper()
	empty := &node.Node{
		InsID:         "node-empty",
		QuotaCpu:      1000,
		QuotaMem:      1000,
		QuotaCpuUsage: 100,
		QuotaMemUsage: 100,
		MvmNum:        1,
		MaxMvmLimit:   10,
	}
	full := &node.Node{
		InsID:         "node-full",
		QuotaCpu:      1000,
		QuotaMem:      1000,
		QuotaCpuUsage: 800,
		QuotaMemUsage: 800,
		MvmNum:        8,
		MaxMvmLimit:   10,
	}
	selCtx := selctx.New("random")
	selCtx.Ctx = context.Background()
	selCtx.SetNodes(node.NodeList{empty, full})
	return selCtx
}
