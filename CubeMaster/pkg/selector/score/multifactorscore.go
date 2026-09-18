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
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

// multiFactorWeightedAverageScore 多因子加权平均评分插件：
// 直接使用后台协程（loopAsyncScore）异步计算并写回的节点 Score 字段作为本插件得分
type multiFactorWeightedAverageScore struct {
	weight float64
}

func multiFactorWeightedAverageConf() *config.MultiFactorWeightedAverage {
	sched := config.GetConfig().Scheduler
	if sched == nil || sched.Score == nil {
		return nil
	}
	return sched.Score.ScorePluginConf.MultiFactorWeightedAverage
}

// NewMultiFactorWeightedAverageScore tolerates a missing legacy plugin_conf
// block: the scorer then has no weight of its own (a profile entry must carry
// one) and Select stays a no-op until the block is configured, because the
// async score loop only runs off the legacy config tree.
func NewMultiFactorWeightedAverageScore() *multiFactorWeightedAverageScore {
	conf := multiFactorWeightedAverageConf()
	if conf == nil {
		CubeLog.Warnf("scheduler.score.plugin_conf.multi_factor_weighted_average is not configured; multi_factor_weighted_average scores nothing until it is")
		return &multiFactorWeightedAverageScore{}
	}
	return &multiFactorWeightedAverageScore{
		weight: conf.Weight,
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
	conf := multiFactorWeightedAverageConf()
	return conf == nil || conf.Disable
}

// Select 将每个候选节点已异步计算好的 Score 原样包装为评分结果返回
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
