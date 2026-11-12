// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package score provides the score of a node.
package score

import (
	"context"
	"errors"
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

// ErrNotApplicable is returned (possibly wrapped) by a score plugin whose
// dimension genuinely does not apply to the current request. The profile
// pipeline treats it as an explicit skip: the plugin contributes no scores
// and no weight, and it is not treated as a failure even for a ForceEnabled
// plugin under the fail-closed or default-score policies. This is distinct
// from returning an empty list with a nil error, which a ForceEnabled plugin
// is not allowed to do (every candidate must be scored).
var ErrNotApplicable = errors.New("score plugin not applicable to this request")

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

	StartAsyncScore(ctx)
	return ss
}

// StartAsyncScore starts the legacy background score refresher when enabled.
// The profile-based scheduler builds score plugins one by one through the
// unified registry, so background initialization is kept as an explicit hook.
func StartAsyncScore(ctx context.Context) {
	conf := config.GetConfig().Scheduler
	if conf == nil || conf.Score == nil || conf.Score.ScorePluginConf.MultiFactorWeightedAverage == nil {
		return
	}
	recov.GoWithRecover(func() {
		loopAsyncScore(ctx)
	})
}

var scores = map[string]interface{}{
	"real_time_weighted_average":    NewRealTimeWeightedAverageScore,
	"multi_factor_weighted_average": NewMultiFactorWeightedAverageScore,
	"affinity_score":                NewAffinityScore,
	"image_score":                   NewImageScore,
}
