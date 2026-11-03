// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestInjectFactorySchedulerProfilesOnEmptyScheduler verifies that a
// zero-scheduling config injects the three built-in profiles together with
// the score subtree their scorers depend on.
func TestInjectFactorySchedulerProfilesOnEmptyScheduler(t *testing.T) {
	cfg := &Config{}
	assert.NoError(t, preHandleScheduler(cfg))

	sched := cfg.Scheduler
	if !assert.Len(t, sched.Profiles, 3) {
		return
	}
	byName := make(map[string]SchedulerProfileConf, len(sched.Profiles))
	defaults := 0
	for _, p := range sched.Profiles {
		byName[p.Name] = p
		if p.Default {
			defaults++
			assert.Empty(t, p.Route.InstanceTypes, "default profile must not define a route")
			assert.Empty(t, p.Route.Labels, "default profile must not define a route")
		}
	}
	assert.Equal(t, 1, defaults, "factory profiles must contain exactly one default")
	assert.True(t, byName["mixed_binpack"].Default)
	assert.Equal(t, map[string]string{"workload": "burst_balance"}, byName["burst_balance"].Route.Labels)
	assert.Equal(t, map[string]string{"workload": "template_reuse"}, byName["template_reuse"].Route.Labels)
	assert.Equal(t, []string{"workload"}, sched.ProfileRouteLabelKeys)

	burstBalance := byName["burst_balance"]
	if assert.Len(t, burstBalance.Scores, 1) {
		assert.Equal(t, "real_time_weighted_average", burstBalance.Scores[0].Name)
		assert.InDelta(t, 1.0, burstBalance.Scores[0].Weight, 1e-9)
	}
	assert.Equal(t, "spread", burstBalance.Selection.Method)
	assert.Equal(t, 3, burstBalance.Selection.TopN)

	templateReuseWeights := make(map[string]float64)
	for _, scorer := range byName["template_reuse"].Scores {
		templateReuseWeights[scorer.Name] = scorer.Weight
	}
	assert.Len(t, byName["template_reuse"].Scores, 2)
	assert.InDelta(t, 0.7, templateReuseWeights["image_score"], 1e-9)
	assert.InDelta(t, 0.3, templateReuseWeights["real_time_weighted_average"], 1e-9)

	// The built-in scorers' constructors read the global plugin_conf and panic
	// at startup when it is missing, so the score subtree must be injected
	// along with the factory policy; enable_scorers stays empty so the legacy
	// scoring pipeline is not activated.
	if assert.NotNil(t, sched.Score) {
		assert.NotNil(t, sched.Score.ScorePluginConf.RealTimeWeightedAverage)
		assert.NotNil(t, sched.Score.ScorePluginConf.ImageScore)
		assert.NotEmpty(t, sched.Score.ResourceWeights)
		assert.Empty(t, sched.Score.EnableScorers)
	}
}

// TestInjectFactorySchedulerProfilesRespectsUserConfig verifies that the
// factory policy is skipped entirely when the user configured any explicit
// policy (profiles or legacy filter/score).
func TestInjectFactorySchedulerProfilesRespectsUserConfig(t *testing.T) {
	tests := []struct {
		name  string
		sched SchedulerConf
	}{
		{
			name: "user profiles",
			sched: SchedulerConf{
				Profiles: []SchedulerProfileConf{{Name: "mine", Default: true}},
			},
		},
		{
			name:  "user legacy filter",
			sched: SchedulerConf{Filter: &SchedulerFilterConf{EnableFilters: []string{"cpu"}}},
		},
		{
			name: "user legacy score",
			sched: SchedulerConf{Score: &SchedulerScoreConf{
				ResourceWeights: map[string]float64{"quota_cpu_usage": 1},
			}},
		},
		{
			name:  "factory injection disabled",
			sched: SchedulerConf{DisableFactoryProfiles: true},
		},
		{
			name:  "user postscore",
			sched: SchedulerConf{PostScore: &PostScoreConf{}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := &Config{Scheduler: &WrapperSchedulerConf{SchedulerConf: test.sched}}
			assert.NoError(t, preHandleScheduler(cfg))
			if len(test.sched.Profiles) == 0 {
				assert.Empty(t, cfg.Scheduler.Profiles)
			} else {
				if assert.Len(t, cfg.Scheduler.Profiles, 1) {
					assert.Equal(t, "mine", cfg.Scheduler.Profiles[0].Name)
				}
			}
			// The factory score subtree must not be injected either.
			if cfg.Scheduler.Score != nil {
				assert.Nil(t, cfg.Scheduler.Score.ScorePluginConf.RealTimeWeightedAverage)
				assert.Nil(t, cfg.Scheduler.Score.ScorePluginConf.ImageScore)
			}
		})
	}
}
