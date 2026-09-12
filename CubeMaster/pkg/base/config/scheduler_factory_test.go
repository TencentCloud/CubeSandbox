// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestInjectFactorySchedulerProfilesOnEmptyScheduler 验证零调度配置时
// 注入三条内置策略及其 scorer 依赖的 score 子树
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

	// BurstBalance 通过实时资源水位 + 创建并发双 scorer 与 Top-3 spread
	// 组合，把突发创建压力在候选节点间打散
	burstBalance := byName["burst_balance"]
	if assert.Len(t, burstBalance.Scores, 2) {
		assert.Equal(t, "real_time_weighted_average", burstBalance.Scores[0].Name)
		assert.InDelta(t, 1.0, burstBalance.Scores[0].Weight, 1e-9)
		assert.Equal(t, "create_concurrency_score", burstBalance.Scores[1].Name)
		assert.InDelta(t, 1.0, burstBalance.Scores[1].Weight, 1e-9)
	}
	assert.Equal(t, "spread", burstBalance.Selection.Method)
	assert.Equal(t, 3, burstBalance.Selection.TopN)

	// TemplateReuse 以模板本地性为主目标，权重经 schedsim 多 seed 对照
	// 实验裁决（见 docs/dev/scheduler-sim-report.md 的 template_reuse
	// 权重对照章节）；template_local_pressure 必须存在，用于分散同模板
	// 在途创建压力，避免副本节点排队羊群
	templateReuseWeights := make(map[string]float64)
	for _, scorer := range byName["template_reuse"].Scores {
		templateReuseWeights[scorer.Name] = scorer.Weight
	}
	assert.Len(t, byName["template_reuse"].Scores, 3)
	assert.InDelta(t, 0.7, templateReuseWeights["image_score"], 1e-9)
	assert.InDelta(t, 0.2, templateReuseWeights["template_local_pressure"], 1e-9)
	assert.InDelta(t, 0.3, templateReuseWeights["real_time_weighted_average"], 1e-9)

	// 内置 scorer 的构造函数会读全局 plugin_conf，缺失时启动即 panic，
	// 所以 score 子树必须随出厂策略一并注入；enable_scorers 保持为空，
	// 不激活 legacy 评分流水线
	if assert.NotNil(t, sched.Score) {
		assert.NotNil(t, sched.Score.ScorePluginConf.RealTimeWeightedAverage)
		assert.NotNil(t, sched.Score.ScorePluginConf.ImageScore)
		assert.NotEmpty(t, sched.Score.ResourceWeights)
		assert.Empty(t, sched.Score.EnableScorers)
	}
}

// TestInjectFactorySchedulerProfilesRespectsUserConfig 验证用户显式配置
// （profiles 或 legacy filter/score 任意其一）时出厂策略整体不注入
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
			// 出厂 score 子树也不得注入
			if cfg.Scheduler.Score != nil {
				assert.Nil(t, cfg.Scheduler.Score.ScorePluginConf.RealTimeWeightedAverage)
				assert.Nil(t, cfg.Scheduler.Score.ScorePluginConf.ImageScore)
			}
		})
	}
}
