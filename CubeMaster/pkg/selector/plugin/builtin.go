// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package plugin

import (
	"context"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/filter"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/score"
)

// RegisterBuiltins exposes the legacy in-process plugins through the unified
// registry. Registration is explicit so tests and embedding applications can
// build isolated registries without mutating package globals.
func RegisterBuiltins(registry *Registry) error {
	filters := map[string]FilterFactory{
		"node_safety": func(context.Context, config.SchedulerProfilePluginConf) (filter.Selector, error) {
			return filter.NewNodeSafetyFilter(), nil
		},
		"cpu": func(context.Context, config.SchedulerProfilePluginConf) (filter.Selector, error) {
			return filter.NewCpuFilter(), nil
		},
		"mem": func(context.Context, config.SchedulerProfilePluginConf) (filter.Selector, error) {
			return filter.NewMemFilter(), nil
		},
		"template_locality": func(context.Context, config.SchedulerProfilePluginConf) (filter.Selector, error) {
			return filter.NewTemplateLocalityFilter(), nil
		},
		"realtime_create_num": func(context.Context, config.SchedulerProfilePluginConf) (filter.Selector, error) {
			return filter.NewRealtimecreatelimit(), nil
		},
		"disk": func(context.Context, config.SchedulerProfilePluginConf) (filter.Selector, error) {
			return filter.NewDiskFilter(), nil
		},
		"thirtparty": func(context.Context, config.SchedulerProfilePluginConf) (filter.Selector, error) {
			return filter.NewThirtpartyFilter(), nil
		},
	}
	for name, factory := range filters {
		if err := registry.RegisterFilter(TypeGo, name, factory); err != nil {
			return err
		}
	}

	scores := map[string]ScoreFactory{
		"real_time_weighted_average": func(context.Context, config.SchedulerProfilePluginConf) (score.Selector, error) {
			return score.NewRealTimeWeightedAverageScore(), nil
		},
		"multi_factor_weighted_average": func(context.Context, config.SchedulerProfilePluginConf) (score.Selector, error) {
			return score.NewMultiFactorWeightedAverageScore(), nil
		},
		"affinity_score": func(context.Context, config.SchedulerProfilePluginConf) (score.Selector, error) {
			return score.NewAffinityScore(), nil
		},
		"image_score": func(context.Context, config.SchedulerProfilePluginConf) (score.Selector, error) {
			return score.NewImageScore(), nil
		},
	}
	for name, factory := range scores {
		if err := registry.RegisterScore(TypeGo, name, factory); err != nil {
			return err
		}
	}
	return nil
}
