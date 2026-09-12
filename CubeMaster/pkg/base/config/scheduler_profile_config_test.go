// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestPreHandleScheduler_NoProfileLeavesSchedulerUnchanged(t *testing.T) {
	cfg := &Config{Scheduler: &WrapperSchedulerConf{
		SchedulerConf: SchedulerConf{
			Filter: &SchedulerFilterConf{EnableFilters: []string{"cpu", "mem"}},
			Score: &SchedulerScoreConf{
				EnableScorers:   []string{"affinity_score"},
				ResourceWeights: map[string]float64{"cpu": 1.0},
				ScorePluginConf: ScorePluginConf{
					AffinityScore: &AffinityScore{Weight: 1},
				},
			},
			Profiles: map[string]SchedulerProfileConf{
				"unused": {
					Filter: &SchedulerFilterConf{EnableFilters: []string{"disk"}},
				},
			},
		},
	}}

	err := preHandleScheduler(cfg)
	assert.NoError(t, err)
	assert.Equal(t, []string{"cpu", "mem"}, cfg.Scheduler.Filter.EnableFilters)
	assert.Equal(t, []string{"affinity_score"}, cfg.Scheduler.Score.EnableScorers)
	assert.Equal(t, map[string]float64{"cpu": 1.0}, cfg.Scheduler.Score.ResourceWeights)
}

func TestPreHandleScheduler_ProfileAppliesFilterAndScore(t *testing.T) {
	cfg := &Config{Scheduler: &WrapperSchedulerConf{
		SchedulerConf: SchedulerConf{
			Profile: "spread_like",
			Filter:  &SchedulerFilterConf{EnableFilters: []string{"cpu"}},
			Score: &SchedulerScoreConf{
				EnableScorers:   []string{"affinity_score"},
				ResourceWeights: map[string]float64{"cpu_util": 1.0, "disk": 7.0},
				ScorePluginConf: ScorePluginConf{
					RealTimeWeightedAverage: &RealTimeWeightedAverage{
						Weight:              1,
						EnableWeightFactors: []string{"realtime_create_num"},
					},
					MultiFactorWeightedAverage: &MultiFactorWeightedAverage{
						Weight:              1,
						ScoreInterval:       time.Second,
						EnableWeightFactors: []string{"mvm_num"},
					},
				},
			},
			Profiles: map[string]SchedulerProfileConf{
				"spread_like": {
					Filter: &SchedulerFilterConf{
						EnableFilters: []string{"cpu", "mem", "realtime_create_num"},
					},
					Score: &SchedulerProfileScoreConf{
						EnableScorers:   []string{"real_time_weighted_average", "multi_factor_weighted_average"},
						ResourceWeights: map[string]float64{"realtime_create_num": 0.4, "mvm_num": 0.6},
					},
				},
			},
		},
	}}

	err := preHandleScheduler(cfg)
	assert.NoError(t, err)
	assert.Equal(t, []string{"cpu", "mem", "realtime_create_num"}, cfg.Scheduler.Filter.EnableFilters)
	assert.Equal(t, []string{"real_time_weighted_average", "multi_factor_weighted_average"}, cfg.Scheduler.Score.EnableScorers)
	assert.Equal(t, map[string]float64{"cpu_util": 1.0, "realtime_create_num": 0.4, "mvm_num": 0.6, "disk": 7.0}, cfg.Scheduler.Score.ResourceWeights)
}

func TestPreHandleScheduler_ProfileWeightsMergeWithNilBase(t *testing.T) {
	cfg := &Config{Scheduler: &WrapperSchedulerConf{
		SchedulerConf: SchedulerConf{
			Profile: "weights_only",
			Profiles: map[string]SchedulerProfileConf{
				"weights_only": {
					Score: &SchedulerProfileScoreConf{
						ResourceWeights: map[string]float64{"cpu": 0.4},
					},
				},
			},
		},
	}}

	err := preHandleScheduler(cfg)
	assert.NoError(t, err)
	assert.Equal(t, map[string]float64{"cpu": 0.4}, cfg.Scheduler.Score.ResourceWeights)
}

func TestPreHandleScheduler_ProfileWithoutWeightsLeavesBaseWeights(t *testing.T) {
	baseWeights := map[string]float64{"cpu": 1, "mem": 2}
	cfg := &Config{Scheduler: &WrapperSchedulerConf{
		SchedulerConf: SchedulerConf{
			Profile: "scorers_only",
			Score: &SchedulerScoreConf{
				ResourceWeights: baseWeights,
				ScorePluginConf: ScorePluginConf{
					BinpackScore: &BinpackScore{Weight: Float64Ptr(1)},
				},
			},
			Profiles: map[string]SchedulerProfileConf{
				"scorers_only": {
					Score: &SchedulerProfileScoreConf{
						EnableScorers: []string{"binpack_score"},
					},
				},
			},
		},
	}}

	err := preHandleScheduler(cfg)
	assert.NoError(t, err)
	assert.Equal(t, baseWeights, cfg.Scheduler.Score.ResourceWeights)
}

func TestPreHandleScheduler_UnknownProfileReturnsError(t *testing.T) {
	cfg := &Config{Scheduler: &WrapperSchedulerConf{
		SchedulerConf: SchedulerConf{
			Profile: "missing_profile",
			Profiles: map[string]SchedulerProfileConf{
				"other": {},
			},
		},
	}}

	err := preHandleScheduler(cfg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "missing_profile")
	assert.Contains(t, err.Error(), "not found")
}

func TestPreHandleScheduler_UnknownFilterInProfileReturnsError(t *testing.T) {
	cfg := &Config{Scheduler: &WrapperSchedulerConf{
		SchedulerConf: SchedulerConf{
			Profile: "bad_filter",
			Profiles: map[string]SchedulerProfileConf{
				"bad_filter": {
					Filter: &SchedulerFilterConf{EnableFilters: []string{"cpu", "not_a_real_filter"}},
				},
			},
		},
	}}

	err := preHandleScheduler(cfg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not_a_real_filter")
	assert.Contains(t, err.Error(), "unknown filter")
}

func TestPreHandleScheduler_UnknownScoreInProfileReturnsError(t *testing.T) {
	cfg := &Config{Scheduler: &WrapperSchedulerConf{
		SchedulerConf: SchedulerConf{
			Profile: "bad_score",
			Profiles: map[string]SchedulerProfileConf{
				"bad_score": {
					Score: &SchedulerProfileScoreConf{
						EnableScorers: []string{"affinity_score", "not_a_real_score"},
					},
				},
			},
		},
	}}

	err := preHandleScheduler(cfg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not_a_real_score")
	assert.Contains(t, err.Error(), "unknown score")
}

func TestPreHandleScheduler_ProfileFilterOnlyDoesNotClearScore(t *testing.T) {
	cfg := &Config{Scheduler: &WrapperSchedulerConf{
		SchedulerConf: SchedulerConf{
			Profile: "filter_only",
			Filter:  &SchedulerFilterConf{EnableFilters: []string{"cpu"}},
			Score: &SchedulerScoreConf{
				EnableScorers:   []string{"image_score"},
				ResourceWeights: map[string]float64{"image_id": 2.0},
				ScorePluginConf: ScorePluginConf{
					ImageScore: &ImageScore{
						Weight:              1,
						EnableWeightFactors: []string{"image_id"},
					},
				},
			},
			Profiles: map[string]SchedulerProfileConf{
				"filter_only": {
					Filter: &SchedulerFilterConf{EnableFilters: []string{"cpu", "disk"}},
				},
			},
		},
	}}

	err := preHandleScheduler(cfg)
	assert.NoError(t, err)
	assert.Equal(t, []string{"cpu", "disk"}, cfg.Scheduler.Filter.EnableFilters)
	assert.Equal(t, []string{"image_score"}, cfg.Scheduler.Score.EnableScorers)
	assert.Equal(t, map[string]float64{"image_id": 2.0}, cfg.Scheduler.Score.ResourceWeights)
}

func TestPreHandleScheduler_ProfileScoreOnlyDoesNotClearFilter(t *testing.T) {
	cfg := &Config{Scheduler: &WrapperSchedulerConf{
		SchedulerConf: SchedulerConf{
			Profile: "score_only",
			Filter:  &SchedulerFilterConf{EnableFilters: []string{"mem", "thirtparty"}},
			Score: &SchedulerScoreConf{
				EnableScorers:   []string{"affinity_score"},
				ResourceWeights: map[string]float64{"cpu": 1.0},
				ScorePluginConf: ScorePluginConf{
					BinpackScore: &BinpackScore{Weight: Float64Ptr(1)},
				},
			},
			Profiles: map[string]SchedulerProfileConf{
				"score_only": {
					Score: &SchedulerProfileScoreConf{
						EnableScorers:   []string{"binpack_score"},
						ResourceWeights: map[string]float64{"cpu": 0.2, "mem": 0.8},
					},
				},
			},
		},
	}}

	err := preHandleScheduler(cfg)
	assert.NoError(t, err)
	assert.Equal(t, []string{"mem", "thirtparty"}, cfg.Scheduler.Filter.EnableFilters)
	assert.Equal(t, []string{"binpack_score"}, cfg.Scheduler.Score.EnableScorers)
	assert.Equal(t, map[string]float64{"cpu": 0.2, "mem": 0.8}, cfg.Scheduler.Score.ResourceWeights)
}

func TestAllowedSchedulerSelectorNamesDocumented(t *testing.T) {
	// Cross-package drift vs filter/score registries is enforced in
	// pkg/scheduler.TestSelectorAllowlistsMatchRegistries. This test only
	// guards that the allowlists stay non-empty and contain the built-ins
	// this PR relies on.
	assert.Contains(t, allowedSchedulerFilterNames, "cpu")
	assert.Contains(t, allowedSchedulerFilterNames, "thirtparty")
	assert.Contains(t, allowedSchedulerScoreNames, "binpack_score")
	assert.Contains(t, allowedSchedulerScoreNames, "external_http_score")
	assert.Contains(t, allowedSchedulerScoreNames, "real_time_weighted_average")
	assert.Equal(t, 6, len(allowedSchedulerFilterNames))
	assert.Equal(t, 6, len(allowedSchedulerScoreNames))
}

func TestScorerPluginValidationCoversAllowlist(t *testing.T) {
	// A newly allowlisted scorer that is missing from scorerPluginConfMissing /
	// scorerPluginExplicitlyDisabled / isFactorBasedSchedulerScore would
	// compile and pass the registry drift test while silently skipping
	// plugin_conf fail-closed checks. Probe every allowlisted name.
	assert.Equal(t, allowedSchedulerScoreNames, ScorerNamesWithPluginConfMissingCheck())

	wantFactorBased := map[string]bool{
		"real_time_weighted_average":    true,
		"multi_factor_weighted_average": true,
		"affinity_score":                false,
		"image_score":                   true,
		"binpack_score":                 false,
		"external_http_score":           false,
	}
	assert.Equal(t, len(allowedSchedulerScoreNames), len(wantFactorBased))
	for name := range allowedSchedulerScoreNames {
		want, ok := wantFactorBased[name]
		assert.True(t, ok, "wantFactorBased missing %q", name)
		assert.Equal(t, want, isFactorBasedSchedulerScore(name), name)
	}

	for name := range allowedSchedulerScoreNames {
		cfg := schedulerConfWithScorerExplicitlyDisabled(t, name)
		assert.True(t, scorerPluginExplicitlyDisabled(cfg, name),
			"scorerPluginExplicitlyDisabled does not recognize %q", name)
	}
}

func schedulerConfWithScorerExplicitlyDisabled(t *testing.T, name string) *SchedulerConf {
	t.Helper()
	s := &SchedulerConf{Score: &SchedulerScoreConf{}}
	switch name {
	case "real_time_weighted_average":
		s.Score.ScorePluginConf.RealTimeWeightedAverage = &RealTimeWeightedAverage{Disable: true}
	case "multi_factor_weighted_average":
		s.Score.ScorePluginConf.MultiFactorWeightedAverage = &MultiFactorWeightedAverage{Disable: true}
	case "affinity_score":
		s.Score.ScorePluginConf.AffinityScore = &AffinityScore{Disable: true}
	case "image_score":
		s.Score.ScorePluginConf.ImageScore = &ImageScore{Disable: true}
	case "binpack_score":
		s.Score.ScorePluginConf.BinpackScore = &BinpackScore{Disable: true}
	case "external_http_score":
		s.Score.ScorePluginConf.ExternalHTTPScore = &ExternalHTTPScore{Disable: true}
	default:
		t.Fatalf("add disabled probe fixture for scorer %q", name)
	}
	return s
}

func initConfigFromYAML(t *testing.T, yamlBody string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cubemaster.yaml")
	if err := os.WriteFile(path, []byte(yamlBody), 0644); err != nil {
		t.Fatalf("write config yaml: %v", err)
	}
	t.Setenv("CUBE_MASTER_CONFIG_PATH", path)
	old := cfg
	t.Cleanup(func() { cfg = old })
	return Init()
}

func TestInit_EmptySchedulerProfileLeavesDirectConfigUnchanged(t *testing.T) {
	yamlBody := `common: {}
log: {}
scheduler:
  filter:
    enable_filters:
      - cpu
      - mem
  score:
    enable_scorers:
      - affinity_score
    resource_weights:
      cpu: 1
    plugin_conf:
      affinity_score:
        weight: 1
      binpack_score:
        weight: 1
        cpu_weight: 2
        mem_weight: 3
        mvm_weight: 4
        disable: false
  profiles:
    unused:
      filter:
        enable_filters:
          - disk
`
	got, err := initConfigFromYAML(t, yamlBody)
	assert.NoError(t, err)
	assert.NotNil(t, got)
	assert.Equal(t, "", got.Scheduler.Profile)
	assert.Equal(t, []string{"cpu", "mem"}, got.Scheduler.Filter.EnableFilters)
	assert.Equal(t, []string{"affinity_score"}, got.Scheduler.Score.EnableScorers)
	assert.Equal(t, map[string]float64{"cpu": 1}, got.Scheduler.Score.ResourceWeights)
	plugin := got.Scheduler.Score.ScorePluginConf.BinpackScore
	if assert.NotNil(t, plugin) {
		w, disabled := BinpackPluginWeight(plugin)
		assert.Equal(t, 1.0, w)
		assert.False(t, disabled)
		assert.Equal(t, 2.0, plugin.CPUWeight)
		assert.Equal(t, 3.0, plugin.MemWeight)
		assert.Equal(t, 4.0, plugin.MvmWeight)
		assert.False(t, plugin.Disable)
	}
}

func TestPreHandleScheduler_BuiltinProfilesApplyWithoutUserMap(t *testing.T) {
	cases := []struct {
		name            string
		wantFilters     []string
		wantScorers     []string
		wantWeightKey   string
		wantWeightValue float64
	}{
		{
			name:            "balanced_spread",
			wantFilters:     []string{"cpu", "mem", "realtime_create_num"},
			wantScorers:     []string{"real_time_weighted_average"},
			wantWeightKey:   "realtime_create_num",
			wantWeightValue: 2,
		},
		{
			name:            "template_locality_first",
			wantFilters:     []string{"cpu", "mem", "template_locality"},
			wantScorers:     []string{"image_score"},
			wantWeightKey:   "template_id",
			wantWeightValue: 2,
		},
		{
			name:            "binpack_utilization",
			wantFilters:     []string{"cpu", "mem"},
			wantScorers:     []string{"binpack_score"},
			wantWeightKey:   "",
			wantWeightValue: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			yamlBody := fmt.Sprintf(`common: {}
log: {}
scheduler:
  profile: %s
`, tc.name)
			got, err := initConfigFromYAML(t, yamlBody)
			assert.NoError(t, err)
			assert.NotNil(t, got)
			assert.Equal(t, tc.name, got.Scheduler.Profile)
			assert.Equal(t, tc.wantFilters, got.Scheduler.Filter.EnableFilters)
			assert.Equal(t, tc.wantScorers, got.Scheduler.Score.EnableScorers)
			if tc.wantWeightKey == "" {
				assert.NotContains(t, got.Scheduler.Score.ResourceWeights, "binpack_score")
			} else {
				assert.Equal(t, tc.wantWeightValue, got.Scheduler.Score.ResourceWeights[tc.wantWeightKey])
			}
			switch tc.name {
			case RuntimeProfileBalancedSpread:
				assert.NotNil(t, got.Scheduler.Score.ScorePluginConf.RealTimeWeightedAverage)
			case RuntimeProfileTemplateLocalityFirst:
				assert.NotNil(t, got.Scheduler.Score.ScorePluginConf.ImageScore)
			case RuntimeProfileBinpackUtilization:
				binpack := got.Scheduler.Score.ScorePluginConf.BinpackScore
				if assert.NotNil(t, binpack) {
					w, disabled := BinpackPluginWeight(binpack)
					assert.Equal(t, 1.0, w)
					assert.False(t, disabled)
					assert.Equal(t, 1.0, binpack.CPUWeight)
					assert.Equal(t, 1.0, binpack.MemWeight)
					assert.Equal(t, 1.0, binpack.MvmWeight)
				}
			}
		})
	}
}

func TestPreHandleScheduler_UnknownProfileStillFailsClosed(t *testing.T) {
	yamlBody := `common: {}
log: {}
scheduler:
  profile: not_a_builtin_or_user_profile
  profiles:
    http_score_combo:
      filter:
        enable_filters:
          - cpu
`
	_, err := initConfigFromYAML(t, yamlBody)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not_a_builtin_or_user_profile")
	assert.Contains(t, err.Error(), "not found")
}

func TestPreHandleScheduler_UserProfileOverridesBuiltin(t *testing.T) {
	yamlBody := `common: {}
log: {}
scheduler:
  profile: balanced_spread
  score:
    resource_weights:
      mem: 3
    plugin_conf:
      affinity_score:
        weight: 1
  profiles:
    balanced_spread:
      filter:
        enable_filters:
          - disk
      score:
        enable_scorers:
          - affinity_score
        resource_weights:
          cpu: 9
`
	got, err := initConfigFromYAML(t, yamlBody)
	assert.NoError(t, err)
	assert.Equal(t, []string{"disk"}, got.Scheduler.Filter.EnableFilters)
	assert.Equal(t, []string{"affinity_score"}, got.Scheduler.Score.EnableScorers)
	assert.Equal(t, map[string]float64{"cpu": 9, "mem": 3}, got.Scheduler.Score.ResourceWeights)
	assert.Nil(t, got.Scheduler.Score.ScorePluginConf.RealTimeWeightedAverage)
}

func TestInit_UserProfileRequiredPluginConfigFailsFast(t *testing.T) {
	for _, scorer := range []string{
		"real_time_weighted_average",
		"multi_factor_weighted_average",
		"affinity_score",
		"image_score",
	} {
		t.Run(scorer, func(t *testing.T) {
			yamlBody := fmt.Sprintf(`common: {}
log: {}
scheduler:
  profile: missing_plugin
  profiles:
    missing_plugin:
      score:
        enable_scorers:
          - %s
`, scorer)
			_, err := initConfigFromYAML(t, yamlBody)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "plugin_conf."+scorer)
			assert.Contains(t, err.Error(), "is missing")
		})
	}
}

func TestInit_UserProfileBinpackMayOmitPluginConf(t *testing.T) {
	// Consistent with empty-Profile: binpack_score may omit plugin_conf and
	// use runtime defaults. Built-in presets still inject when the pointer is nil.
	yamlBody := `common: {}
log: {}
scheduler:
  profile: user_binpack
  profiles:
    user_binpack:
      score:
        enable_scorers:
          - binpack_score
`
	got, err := initConfigFromYAML(t, yamlBody)
	assert.NoError(t, err)
	assert.Equal(t, []string{"binpack_score"}, got.Scheduler.Score.EnableScorers)
	assert.Nil(t, got.Scheduler.Score.ScorePluginConf.BinpackScore)
}

func TestInit_ProfileDroppingBaseFiltersFailsClosed(t *testing.T) {
	yamlBody := `common: {}
log: {}
scheduler:
  profile: drop_disk
  filter:
    enable_filters:
      - cpu
      - mem
      - disk
  profiles:
    drop_disk:
      filter:
        enable_filters:
          - cpu
          - mem
`
	_, err := initConfigFromYAML(t, yamlBody)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "drops filters")
	assert.Contains(t, err.Error(), "disk")
	assert.Contains(t, err.Error(), "allow_dropped_filters")
}

func TestInit_ProfileAllowDroppedFiltersOptIn(t *testing.T) {
	yamlBody := `common: {}
log: {}
scheduler:
  profile: drop_disk
  filter:
    enable_filters:
      - cpu
      - mem
      - disk
  profiles:
    drop_disk:
      allow_dropped_filters: true
      filter:
        enable_filters:
          - cpu
          - mem
`
	got, err := initConfigFromYAML(t, yamlBody)
	assert.NoError(t, err)
	assert.Equal(t, []string{"cpu", "mem"}, got.Scheduler.Filter.EnableFilters)
}

func TestInit_EmptySameNameProfileFallsBackToBuiltin(t *testing.T) {
	yamlBody := `common: {}
log: {}
scheduler:
  profile: binpack_utilization
  profiles:
    binpack_utilization:
`
	got, err := initConfigFromYAML(t, yamlBody)
	assert.NoError(t, err)
	assert.Equal(t, []string{"binpack_score"}, got.Scheduler.Score.EnableScorers)
	assert.NotNil(t, got.Scheduler.Score.ScorePluginConf.BinpackScore)
}

func TestInit_OptInOnlySameNameAppliesBuiltin(t *testing.T) {
	// allow_dropped_filters alone must not silent-no-op a built-in name.
	yamlBody := `common: {}
log: {}
scheduler:
  profile: binpack_utilization
  filter:
    enable_filters:
      - cpu
      - mem
      - template_locality
      - realtime_create_num
  profiles:
    binpack_utilization:
      allow_dropped_filters: true
`
	got, err := initConfigFromYAML(t, yamlBody)
	assert.NoError(t, err)
	assert.Equal(t, []string{"cpu", "mem"}, got.Scheduler.Filter.EnableFilters)
	assert.Equal(t, []string{"binpack_score"}, got.Scheduler.Score.EnableScorers)
	assert.NotNil(t, got.Scheduler.Score.ScorePluginConf.BinpackScore)
}

func TestInit_BuiltinOnStockFiltersSucceeds(t *testing.T) {
	// Stock CubeMaster configs enable cpu/mem/template_locality/realtime_create_num.
	// Built-ins carry AllowDroppedFilters so selecting by name still loads.
	for _, profile := range []string{
		"balanced_spread",
		"template_locality_first",
		"binpack_utilization",
	} {
		t.Run(profile, func(t *testing.T) {
			yamlBody := fmt.Sprintf(`common: {}
log: {}
scheduler:
  profile: %s
  filter:
    enable_filters:
      - cpu
      - mem
      - template_locality
      - realtime_create_num
`, profile)
			got, err := initConfigFromYAML(t, yamlBody)
			assert.NoError(t, err)
			assert.Equal(t, profile, got.Scheduler.Profile)
			assert.NotEmpty(t, got.Scheduler.Score.EnableScorers)
		})
	}
}

func TestInit_ProfilePolarityMixRejected(t *testing.T) {
	yamlBody := `common: {}
log: {}
scheduler:
  profile: mixed
  profiles:
    mixed:
      score:
        enable_scorers:
          - binpack_score
          - real_time_weighted_average
        resource_weights:
          mvm_num: 1
  score:
    plugin_conf:
      real_time_weighted_average:
        weight: 1
        enable_weight_factors: [mvm_num]
`
	_, err := initConfigFromYAML(t, yamlBody)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "mixes binpack_score")
	assert.Contains(t, err.Error(), "polarities cancel")
}

func TestInit_DirectEnabledScorerMissingPluginConfigAllowedWithoutProfile(t *testing.T) {
	// Empty-profile: binpack_score may omit plugin_conf and use runtime defaults.
	yamlBody := `common: {}
log: {}
scheduler:
  score:
    enable_scorers:
      - binpack_score
`
	got, err := initConfigFromYAML(t, yamlBody)
	assert.NoError(t, err)
	assert.Equal(t, []string{"binpack_score"}, got.Scheduler.Score.EnableScorers)
	assert.Nil(t, got.Scheduler.Score.ScorePluginConf.BinpackScore)
}

func TestInit_EmptyProfileMissingFactorPluginConfFails(t *testing.T) {
	// Master NewSelector panicked when a listed factor scorer lacked plugin_conf.
	// Fail at config load so the empty-profile path stays fail-closed.
	yamlBody := `common: {}
log: {}
scheduler:
  score:
    enable_scorers:
      - real_time_weighted_average
      - binpack_score
    resource_weights:
      realtime_create_num: 1
    plugin_conf:
      binpack_score:
        weight: 1
`
	_, err := initConfigFromYAML(t, yamlBody)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "real_time_weighted_average")
	assert.Contains(t, err.Error(), "plugin_conf.real_time_weighted_average is missing")
}

func TestInit_ProfileInheritedEnabledScorerMissingPluginConfigFailsFast(t *testing.T) {
	yamlBody := `common: {}
log: {}
scheduler:
  profile: filters_only
  score:
    enable_scorers:
      - image_score
  profiles:
    filters_only:
      filter:
        enable_filters:
          - cpu
`
	_, err := initConfigFromYAML(t, yamlBody)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "image_score")
	assert.Contains(t, err.Error(), "plugin_conf.image_score is missing")
}

func TestInit_EmptySchedulerProfileDoesNotApplyBuiltin(t *testing.T) {
	yamlBody := `common: {}
log: {}
scheduler:
  filter:
    enable_filters:
      - cpu
      - mem
  score:
    enable_scorers:
      - affinity_score
    resource_weights:
      cpu: 1
    plugin_conf:
      affinity_score:
        weight: 1
`
	got, err := initConfigFromYAML(t, yamlBody)
	assert.NoError(t, err)
	assert.Equal(t, "", got.Scheduler.Profile)
	assert.Equal(t, []string{"cpu", "mem"}, got.Scheduler.Filter.EnableFilters)
	assert.Equal(t, []string{"affinity_score"}, got.Scheduler.Score.EnableScorers)
	assert.Equal(t, map[string]float64{"cpu": 1}, got.Scheduler.Score.ResourceWeights)
}

func TestInit_BuiltinProfileConflictsWithExplicitDisableFailsFast(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantSub string
	}{
		{
			name: "balanced_spread_weight0",
			yaml: `common: {}
log: {}
scheduler:
  profile: balanced_spread
  score:
    plugin_conf:
      real_time_weighted_average:
        weight: 0
        enable_weight_factors: [realtime_create_num]
`,
			wantSub: "explicitly disabled",
		},
		{
			name: "balanced_spread_disable_true",
			yaml: `common: {}
log: {}
scheduler:
  profile: balanced_spread
  score:
    plugin_conf:
      real_time_weighted_average:
        weight: 1
        disable: true
        enable_weight_factors: [realtime_create_num]
`,
			wantSub: "explicitly disabled",
		},
		{
			name: "template_locality_first_weight0",
			yaml: `common: {}
log: {}
scheduler:
  profile: template_locality_first
  score:
    plugin_conf:
      image_score:
        weight: 0
        enable_weight_factors: [image_id, template_id]
`,
			wantSub: "explicitly disabled",
		},
		{
			name: "binpack_utilization_disable_true",
			yaml: `common: {}
log: {}
scheduler:
  profile: binpack_utilization
  score:
    plugin_conf:
      binpack_score:
        weight: 1
        disable: true
`,
			wantSub: "explicitly disabled",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := initConfigFromYAML(t, tc.yaml)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantSub)
		})
	}
}

func TestInit_DirectEnableWeightZeroStillAllowed(t *testing.T) {
	// C40: weight:0 disables without requiring a profile conflict fail-fast.
	yamlBody := `common: {}
log: {}
scheduler:
  score:
    enable_scorers:
      - binpack_score
    plugin_conf:
      binpack_score:
        weight: 0
`
	got, err := initConfigFromYAML(t, yamlBody)
	assert.NoError(t, err)
	assert.Equal(t, []string{"binpack_score"}, got.Scheduler.Score.EnableScorers)
	w, disabled := BinpackPluginWeight(got.Scheduler.Score.ScorePluginConf.BinpackScore)
	assert.Equal(t, 0.0, w)
	assert.True(t, disabled)
}

func TestInit_FactorScorerWithoutPositiveFactorWeight_EmptyProfileStillLoads(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{
			name: "realtime_all_factor_weights_zero",
			yaml: `common: {}
log: {}
scheduler:
  score:
    enable_scorers:
      - real_time_weighted_average
    resource_weights:
      realtime_create_num: 0
      mvm_num: 0
    plugin_conf:
      real_time_weighted_average:
        weight: 1
        enable_weight_factors: [realtime_create_num, mvm_num]
`,
		},
		{
			name: "realtime_missing_resource_weights",
			yaml: `common: {}
log: {}
scheduler:
  score:
    enable_scorers:
      - real_time_weighted_average
    plugin_conf:
      real_time_weighted_average:
        weight: 1
        enable_weight_factors: [realtime_create_num]
`,
		},
		{
			name: "image_score_all_factor_weights_zero",
			yaml: `common: {}
log: {}
scheduler:
  score:
    enable_scorers:
      - image_score
    resource_weights:
      image_id: 0
      template_id: 0
    plugin_conf:
      image_score:
        weight: 1
        enable_weight_factors: [image_id, template_id]
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := initConfigFromYAML(t, tc.yaml)
			assert.NoError(t, err)
			assert.Equal(t, "", got.Scheduler.Profile)
		})
	}
}

func TestInit_FactorScorerWithoutPositiveFactorWeight_WithProfileFailsFast(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantSub []string
	}{
		{
			name: "realtime_all_factor_weights_zero",
			yaml: `common: {}
log: {}
scheduler:
  profile: ineffective_realtime
  profiles:
    ineffective_realtime:
      score:
        enable_scorers:
          - real_time_weighted_average
        resource_weights:
          realtime_create_num: 0
          mvm_num: 0
  score:
    plugin_conf:
      real_time_weighted_average:
        weight: 1
        enable_weight_factors: [realtime_create_num, mvm_num]
`,
			wantSub: []string{"ineffective_realtime", "real_time_weighted_average", "no positive resource weight"},
		},
		{
			name: "realtime_missing_resource_weights",
			yaml: `common: {}
log: {}
scheduler:
  profile: missing_weights
  profiles:
    missing_weights:
      score:
        enable_scorers:
          - real_time_weighted_average
  score:
    plugin_conf:
      real_time_weighted_average:
        weight: 1
        enable_weight_factors: [realtime_create_num]
`,
			wantSub: []string{"missing_weights", "real_time_weighted_average", "no positive resource weight"},
		},
		{
			name: "image_score_all_factor_weights_zero",
			yaml: `common: {}
log: {}
scheduler:
  profile: ineffective_image
  profiles:
    ineffective_image:
      score:
        enable_scorers:
          - image_score
        resource_weights:
          image_id: 0
          template_id: 0
  score:
    plugin_conf:
      image_score:
        weight: 1
        enable_weight_factors: [image_id, template_id]
`,
			wantSub: []string{"ineffective_image", "image_score", "no positive resource weight"},
		},
		{
			name: "realtime_empty_enable_weight_factors",
			yaml: `common: {}
log: {}
scheduler:
  profile: empty_factors
  profiles:
    empty_factors:
      score:
        enable_scorers:
          - real_time_weighted_average
        resource_weights:
          realtime_create_num: 2
  score:
    plugin_conf:
      real_time_weighted_average:
        weight: 1
`,
			wantSub: []string{"empty_factors", "real_time_weighted_average", "enable_weight_factors is empty"},
		},
		{
			name: "realtime_explicit_empty_enable_weight_factors",
			yaml: `common: {}
log: {}
scheduler:
  profile: empty_factors_list
  profiles:
    empty_factors_list:
      score:
        enable_scorers:
          - real_time_weighted_average
        resource_weights:
          realtime_create_num: 2
  score:
    plugin_conf:
      real_time_weighted_average:
        weight: 1
        enable_weight_factors: []
`,
			wantSub: []string{"empty_factors_list", "real_time_weighted_average", "enable_weight_factors is empty"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := initConfigFromYAML(t, tc.yaml)
			assert.Error(t, err)
			for _, sub := range tc.wantSub {
				assert.Contains(t, err.Error(), sub)
			}
		})
	}
}

func TestInit_UserProfileExplicitDisableRemainsAllowed(t *testing.T) {
	// User Profiles may enable a scorer name while leaving weight:0 / disable
	// as an intentional no-op. Only built-in presets conflict-fail on that.
	yamlBody := `common: {}
log: {}
scheduler:
  profile: binpack_only
  profiles:
    binpack_only:
      score:
        enable_scorers:
          - binpack_score
  score:
    plugin_conf:
      binpack_score:
        weight: 0
`
	got, err := initConfigFromYAML(t, yamlBody)
	assert.NoError(t, err)
	assert.Equal(t, []string{"binpack_score"}, got.Scheduler.Score.EnableScorers)
	w, disabled := BinpackPluginWeight(got.Scheduler.Score.ScorePluginConf.BinpackScore)
	assert.Equal(t, 0.0, w)
	assert.True(t, disabled)
}

func TestInit_UserSameNameOverrideOfBuiltinWeightZeroAllowed(t *testing.T) {
	// A Profiles map key with the same name as a built-in is treated as a user
	// Profile (not a built-in preset), so weight:0 remains an intentional no-op.
	yamlBody := `common: {}
log: {}
scheduler:
  profile: balanced_spread
  profiles:
    balanced_spread:
      score:
        enable_scorers:
          - real_time_weighted_average
        resource_weights:
          realtime_create_num: 1
  score:
    plugin_conf:
      real_time_weighted_average:
        weight: 0
        enable_weight_factors: [realtime_create_num]
`
	got, err := initConfigFromYAML(t, yamlBody)
	assert.NoError(t, err)
	assert.Equal(t, []string{"real_time_weighted_average"}, got.Scheduler.Score.EnableScorers)
	assert.Equal(t, 0.0, got.Scheduler.Score.ScorePluginConf.RealTimeWeightedAverage.Weight)
}

func TestInit_UnknownEnableWeightFactorFailsUnderProfile(t *testing.T) {
	yamlBody := `common: {}
log: {}
scheduler:
  profile: bad_factor
  profiles:
    bad_factor:
      score:
        enable_scorers:
          - real_time_weighted_average
        resource_weights:
          realtime_create_num: 1
  score:
    plugin_conf:
      real_time_weighted_average:
        weight: 1
        enable_weight_factors: [not_a_real_factor]
`
	_, err := initConfigFromYAML(t, yamlBody)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported weight factor")
	assert.Contains(t, err.Error(), "not_a_real_factor")
}

func TestInit_RealtimeAllowsReqCpuReqMemFactors(t *testing.T) {
	yamlBody := `common: {}
log: {}
scheduler:
  profile: realtime_req
  profiles:
    realtime_req:
      score:
        enable_scorers:
          - real_time_weighted_average
        resource_weights:
          req_cpu: 1
          req_mem: 1
  score:
    plugin_conf:
      real_time_weighted_average:
        weight: 1
        enable_weight_factors: [req_cpu, req_mem]
`
	got, err := initConfigFromYAML(t, yamlBody)
	assert.NoError(t, err)
	assert.Equal(t, []string{"req_cpu", "req_mem"}, got.Scheduler.Score.ScorePluginConf.RealTimeWeightedAverage.EnableWeightFactors)
}

func TestInit_MultiFactorRejectsReqCpuFactor(t *testing.T) {
	yamlBody := `common: {}
log: {}
scheduler:
  profile: mfwa_req
  profiles:
    mfwa_req:
      score:
        enable_scorers:
          - multi_factor_weighted_average
        resource_weights:
          req_cpu: 1
  score:
    plugin_conf:
      multi_factor_weighted_average:
        weight: 1
        enable_weight_factors: [req_cpu]
`
	_, err := initConfigFromYAML(t, yamlBody)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported weight factor")
	assert.Contains(t, err.Error(), "req_cpu")
}

func TestInit_NegativeBinpackCPUWeightRejected(t *testing.T) {
	yamlBody := `common: {}
log: {}
scheduler:
  score:
    enable_scorers:
      - binpack_score
    plugin_conf:
      binpack_score:
        weight: 1
        cpu_weight: -1
`
	_, err := initConfigFromYAML(t, yamlBody)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "binpack_score.cpu_weight must be >= 0")
}

func TestInit_DisabledFactorScorerSkipsFactorWeightGate(t *testing.T) {
	yamlBody := `common: {}
log: {}
scheduler:
  score:
    enable_scorers:
      - real_time_weighted_average
    resource_weights:
      realtime_create_num: 0
    plugin_conf:
      real_time_weighted_average:
        weight: 0
        enable_weight_factors: [realtime_create_num]
`
	got, err := initConfigFromYAML(t, yamlBody)
	assert.NoError(t, err)
	assert.Equal(t, 0.0, got.Scheduler.Score.ScorePluginConf.RealTimeWeightedAverage.Weight)
}

func TestInit_BinpackScoreWeightSemantics(t *testing.T) {
	cases := []struct {
		name         string
		yaml         string
		wantErr      string
		wantWeight   float64
		wantDisabled bool
		wantNilPtr   bool // Weight field is nil (omit), effective weight still wantWeight
		wantNilCfg   bool
	}{
		{
			name: "negative_rejected",
			yaml: `common: {}
log: {}
scheduler:
  score:
    enable_scorers:
      - binpack_score
    plugin_conf:
      binpack_score:
        weight: -1
`,
			wantErr: "binpack_score.weight must be >= 0",
		},
		{
			name: "zero_disables",
			yaml: `common: {}
log: {}
scheduler:
  score:
    enable_scorers:
      - binpack_score
    plugin_conf:
      binpack_score:
        weight: 0
`,
			wantWeight:   0,
			wantDisabled: true,
		},
		{
			name: "positive_kept",
			yaml: `common: {}
log: {}
scheduler:
  score:
    enable_scorers:
      - binpack_score
    plugin_conf:
      binpack_score:
        weight: 2.5
`,
			wantWeight: 2.5,
		},
		{
			name: "omit_weight_defaults_to_one",
			yaml: `common: {}
log: {}
scheduler:
  score:
    enable_scorers:
      - binpack_score
    plugin_conf:
      binpack_score:
        cpu_weight: 2
`,
			wantWeight: 1,
			wantNilPtr: true,
		},
		{
			name: "absent_uses_runtime_default",
			yaml: `common: {}
log: {}
scheduler:
  profile: binpack_utilization
`,
			wantWeight: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := initConfigFromYAML(t, tc.yaml)
			if tc.wantErr != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			assert.NoError(t, err)
			cfg := got.Scheduler.Score.ScorePluginConf.BinpackScore
			if tc.wantNilCfg {
				assert.Nil(t, cfg)
				return
			}
			assert.NotNil(t, cfg)
			if tc.wantNilPtr {
				assert.Nil(t, cfg.Weight)
			}
			w, disabled := BinpackPluginWeight(cfg)
			assert.Equal(t, tc.wantWeight, w)
			assert.Equal(t, tc.wantDisabled, disabled)
		})
	}
}

func TestInit_BuiltinProfilePreservesExplicitPluginConf(t *testing.T) {
	yamlBody := `common: {}
log: {}
scheduler:
  profile: balanced_spread
  score:
    plugin_conf:
      real_time_weighted_average:
        weight: 3
        enable_weight_factors: [cpu_util]
`
	got, err := initConfigFromYAML(t, yamlBody)
	assert.NoError(t, err)
	cfg := got.Scheduler.Score.ScorePluginConf.RealTimeWeightedAverage
	assert.NotNil(t, cfg)
	assert.Equal(t, 3.0, cfg.Weight)
	assert.Equal(t, []string{"cpu_util"}, cfg.EnableWeightFactors)
	// Profile resource_weights still merge; operator factors stay as written.
	assert.Equal(t, 1.0, got.Scheduler.Score.ResourceWeights["cpu_util"])
}

func TestInit_ProfileResourceWeightsSameKeyOverridesBase(t *testing.T) {
	yamlBody := `common: {}
log: {}
scheduler:
  profile: balanced_spread
  score:
    resource_weights:
      mvm_num: 10
      custom_keep: 4
`
	got, err := initConfigFromYAML(t, yamlBody)
	assert.NoError(t, err)
	// Profile same-key override (built-in mvm_num: 2 wins over base 10).
	assert.Equal(t, 2.0, got.Scheduler.Score.ResourceWeights["mvm_num"])
	// Unrelated base keys remain.
	assert.Equal(t, 4.0, got.Scheduler.Score.ResourceWeights["custom_keep"])
}

func TestInit_NegativeScorerPluginWeightsRejected(t *testing.T) {
	cases := []struct {
		name    string
		plugin  string
		yaml    string
		wantErr string
	}{
		{
			name:   "realtime",
			plugin: "real_time_weighted_average",
			yaml: `common: {}
log: {}
scheduler:
  score:
    enable_scorers:
      - real_time_weighted_average
    resource_weights:
      mvm_num: 1
    plugin_conf:
      real_time_weighted_average:
        weight: -1
        enable_weight_factors: [mvm_num]
`,
			wantErr: "real_time_weighted_average.weight must be >= 0",
		},
		{
			name:   "multifactor",
			plugin: "multi_factor_weighted_average",
			yaml: `common: {}
log: {}
scheduler:
  score:
    enable_scorers:
      - multi_factor_weighted_average
    resource_weights:
      mvm_num: 1
    plugin_conf:
      multi_factor_weighted_average:
        weight: -1
        enable_weight_factors: [mvm_num]
`,
			wantErr: "multi_factor_weighted_average.weight must be >= 0",
		},
		{
			name:   "image",
			plugin: "image_score",
			yaml: `common: {}
log: {}
scheduler:
  score:
    enable_scorers:
      - image_score
    resource_weights:
      image_id: 1
    plugin_conf:
      image_score:
        weight: -1
        enable_weight_factors: [image_id]
`,
			wantErr: "image_score.weight must be >= 0",
		},
		{
			name:   "affinity",
			plugin: "affinity_score",
			yaml: `common: {}
log: {}
scheduler:
  score:
    enable_scorers:
      - affinity_score
    resource_weights:
      mvm_num: 1
    plugin_conf:
      affinity_score:
        weight: -1
`,
			wantErr: "affinity_score.weight must be >= 0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := initConfigFromYAML(t, tc.yaml)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
