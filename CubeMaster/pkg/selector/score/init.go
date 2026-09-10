// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package score provides the score of a node.
package score

import (
	"context"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/recov"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

type Selector interface {
	Select(selCtx *selctx.SelectorCtx) (node.NodeScoreList, error)

	ID() string

	Weight() float64

	Disable() bool
}

func NewSelector(ctx context.Context) []Selector {
	conf := config.GetConfig().Scheduler
	if conf == nil || conf.Score == nil || len(conf.Score.EnableScorers) == 0 {
		return []Selector{}
	}
	ss := make([]Selector, 0)
	multiFactorConstructed := false
	for _, name := range conf.Score.EnableScorers {
		registration, ok := scores[name]
		if !ok {
			log.G(ctx).Warnf("unknown scheduler score selector: %s", name)
			continue
		}
		// Legacy affinity_score previously depended on the global
		// ResourceWeights != nil NewSelector guard. Keep that gate only for
		// empty-profile configs so upgrades do not suddenly activate affinity
		// scoring. Plugin-only scorers (e.g. binpack_score) and Profile-selected
		// scorers intentionally skip this gate.
		if registration.legacyRequiresResourceWeights &&
			conf.Score.ResourceWeights == nil &&
			conf.Profile == "" {
			log.G(ctx).Warnf("scheduler score selector %s skipped: resource_weights is nil (legacy empty-profile compatibility)", name)
			continue
		}
		if registration.factors != nil && !hasEffectiveFactorWeight(conf.Score, registration.factors(conf.Score.ScorePluginConf)) {
			log.G(ctx).Warnf("scheduler score selector %s skipped: no positive resource weight for its enabled factors", name)
			continue
		}
		selector := registration.new()
		ss = append(ss, selector)
		if name == "multi_factor_weighted_average" {
			multiFactorConstructed = true
		}
	}

	multiFactorConf := conf.Score.ScorePluginConf.MultiFactorWeightedAverage
	if multiFactorConstructed && multiFactorConf != nil && !multiFactorConf.Disable && multiFactorConf.Weight != 0 {
		recov.GoWithRecover(func() {
			loopAsyncScore(ctx)
		})
	}
	return ss
}

type scoreRegistration struct {
	new                           func() Selector
	factors                       func(config.ScorePluginConf) []string
	legacyRequiresResourceWeights bool
}

func hasEffectiveFactorWeight(conf *config.SchedulerScoreConf, factors []string) bool {
	if conf == nil || len(conf.ResourceWeights) == 0 || len(factors) == 0 {
		return false
	}
	for _, factor := range factors {
		if conf.ResourceWeights[factor] > 0 {
			return true
		}
	}
	return false
}

var scores = map[string]scoreRegistration{
	"real_time_weighted_average": {
		new: func() Selector { return NewRealTimeWeightedAverageScore() },
		factors: func(conf config.ScorePluginConf) []string {
			if conf.RealTimeWeightedAverage == nil {
				return nil
			}
			return conf.RealTimeWeightedAverage.EnableWeightFactors
		},
	},
	"multi_factor_weighted_average": {
		new: func() Selector { return NewMultiFactorWeightedAverageScore() },
		factors: func(conf config.ScorePluginConf) []string {
			if conf.MultiFactorWeightedAverage == nil {
				return nil
			}
			return conf.MultiFactorWeightedAverage.EnableWeightFactors
		},
	},
	"affinity_score": {
		new:                           func() Selector { return NewAffinityScore() },
		legacyRequiresResourceWeights: true,
	},
	"image_score": {
		new: func() Selector { return NewImageScore() },
		factors: func(conf config.ScorePluginConf) []string {
			if conf.ImageScore == nil {
				return nil
			}
			return conf.ImageScore.EnableWeightFactors
		},
	},
	"binpack_score": {
		new: func() Selector { return NewBinpackScore() },
	},
}
