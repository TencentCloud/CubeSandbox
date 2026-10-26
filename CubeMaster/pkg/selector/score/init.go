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

	if conf.Score.ScorePluginConf.MultiFactorWeightedAverage != nil {
		recov.GoWithRecover(func() {
			loopAsyncScore(ctx)
		})
	}
	return ss
}

var scores = map[string]interface{}{
	"real_time_weighted_average":    NewRealTimeWeightedAverageScore,
	"multi_factor_weighted_average": NewMultiFactorWeightedAverageScore,
	"affinity_score":                NewAffinityScore,
	"image_score":                   NewImageScore,
}
