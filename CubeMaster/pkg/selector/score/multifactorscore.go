// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package score

import (
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/ret"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

type multiFactorWeightedAverageScore struct {
	weight float64
}

func NewMultiFactorWeightedAverageScore() *multiFactorWeightedAverageScore {
	if config.GetConfig().Scheduler.Score.ScorePluginConf.MultiFactorWeightedAverage == nil {
		panic("config.Scheduler.Score.ScorePluginConf.AsyncMultiFactor is nil")
	}
	return &multiFactorWeightedAverageScore{
		weight: config.GetConfig().Scheduler.Score.ScorePluginConf.MultiFactorWeightedAverage.Weight,
	}
}

func (l *multiFactorWeightedAverageScore) ID() string {
	return constants.SelectorScoreID + "/" + "multi_factor_weighted_average"
}

func (l *multiFactorWeightedAverageScore) String() string {
	return l.ID()
}

func (l *multiFactorWeightedAverageScore) Weight() float64 {
	return l.weight
}

func (l *multiFactorWeightedAverageScore) Disable() bool {
	return config.GetConfig().Scheduler.Score.ScorePluginConf.MultiFactorWeightedAverage.Disable
}

func (l *multiFactorWeightedAverageScore) Select(selCtx *selctx.SelectorCtx) (nodes node.NodeScoreList,
	err error) {
	defer func() {
		if r := recover(); r != nil {
			err = ret.Errorf(errorcode.ErrorCode_MasterInternalError, "multiFactorWeightedAverageScore panic:%s", r)
		}
	}()
	sconf := config.GetConfig().Scheduler
	if sconf == nil || sconf.Score == nil || sconf.Score.ScorePluginConf.MultiFactorWeightedAverage == nil ||
		sconf.Score.ResourceWeights == nil {
		return nil, nil
	}
	if l.Disable() {
		return nil, nil
	}

	inList := selCtx.Nodes()
	nodes = make(node.NodeScoreList, 0, inList.Len())
	for i := range inList {
		nodes.Append(&node.NodeScore{
			InsID:    inList[i].ID(),
			Score:    inList[i].Score,
			MvmNum:   inList[i].MvmNum,
			OrigNode: inList[i],
		})
	}
	return nodes, nil
}
