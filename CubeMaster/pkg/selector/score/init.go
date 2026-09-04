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

// Selector 评分插件接口：Select 为候选节点打分，Weight 返回插件权重，Disable 表示是否禁用
type Selector interface {
	Select(selCtx *selctx.SelectorCtx) (node.NodeScoreList, error)

	ID() string

	Weight() float64

	Disable() bool
}

// NewSelector 根据配置中启用的评分器名列表构造评分插件；
// 若启用了多因子加权平均评分，同时启动异步评分后台协程预计算节点分数
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

// scores 注册表：评分器配置名 -> 构造函数的映射
var scores = map[string]interface{}{
	"real_time_weighted_average":    NewRealTimeWeightedAverageScore,
	"multi_factor_weighted_average": NewMultiFactorWeightedAverageScore,
	"affinity_score":                NewAffinityScore,
	"image_score":                   NewImageScore,
}
