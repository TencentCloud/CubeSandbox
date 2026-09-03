// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package score provides the score of a node.
package score

import (
	"context"
	"reflect"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
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
	if conf == nil || conf.Score == nil || conf.Score.ResourceWeights == nil || len(conf.Score.EnableScorers) == 0 {
		return []Selector{}
	}
	ss := make([]Selector, 0)
	for _, name := range conf.Score.EnableScorers {

		fn := reflect.ValueOf(scores[name])

		if !fn.IsValid() {
			continue
		}
		ss = append(ss, fn.Call(nil)[0].Interface().(Selector))
	}

	StartAsyncScore(ctx, nil)
	return ss
}

// MultiFactorWeightedAverage is the registry name of the scorer whose node
// scores are maintained by the background async refresher (loopAsyncScore).
const MultiFactorWeightedAverage = "multi_factor_weighted_average"

// StartAsyncScore starts the background score refresher for
// multi_factor_weighted_average when that config section exists. The loop is
// gated per tick on active: while no compiled pipeline binds the scorer
// (active returns false) ticks are skipped, so a configured-but-unbound
// deployment costs one idle goroutine instead of a fleet-wide node-score
// write every ScoreInterval. A nil active means always-on, preserving the
// legacy NewSelector behavior (which only reaches here when enable_scorers
// is non-empty).
func StartAsyncScore(ctx context.Context, active func() bool) {
	conf := config.GetConfig().Scheduler
	if conf == nil || conf.Score == nil || conf.Score.ScorePluginConf.MultiFactorWeightedAverage == nil {
		return
	}
	recov.GoWithRecover(func() {
		loopAsyncScore(ctx, active)
	})
}

var scores = map[string]interface{}{
	"real_time_weighted_average":    NewRealTimeWeightedAverageScore,
	MultiFactorWeightedAverage:      NewMultiFactorWeightedAverageScore,
	"affinity_score":                NewAffinityScore,
	"image_score":                   NewImageScore,
}
