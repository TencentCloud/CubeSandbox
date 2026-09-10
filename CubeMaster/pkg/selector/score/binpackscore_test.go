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
			if cfg.Weight != tt.wantWeight && !tt.wantDisable {
				t.Fatalf("config weight = %v, want %v", cfg.Weight, tt.wantWeight)
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
