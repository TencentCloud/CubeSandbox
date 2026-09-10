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

type affinityScore struct {
	weight float64
}

func NewAffinityScore() *affinityScore {
	if config.GetConfig().Scheduler.Score.ScorePluginConf.AffinityScore == nil {
		panic("config.Scheduler.Score.ScorePluginConf.AffinityScore is nil")
	}
	return &affinityScore{
		weight: config.GetConfig().Scheduler.Score.ScorePluginConf.AffinityScore.Weight,
	}
}

func (l *affinityScore) ID() string {
	return constants.SelectorScoreID + "/" + "affinity_score"
}

func (l *affinityScore) String() string {
	return l.ID()
}

func (l *affinityScore) Weight() float64 {
	cfg := config.GetConfig().Scheduler.Score.ScorePluginConf.AffinityScore
	if cfg == nil || cfg.Disable || cfg.Weight == 0 {
		return 0
	}
	return cfg.Weight
}
func (l *affinityScore) Disable() bool {
	cfg := config.GetConfig().Scheduler.Score.ScorePluginConf.AffinityScore
	return cfg.Disable || cfg.Weight == 0
}

func (l *affinityScore) Select(selCtx *selctx.SelectorCtx) (nodes node.NodeScoreList,
	err error) {
	defer func() {
		if r := recover(); r != nil {
			err = ret.Errorf(errorcode.ErrorCode_MasterInternalError, "affinityScore panic:%s", r)
		}
	}()

	inList := selCtx.Nodes()
	nodes = make(node.NodeScoreList, 0, inList.Len())
	if selCtx.Affinity.NodePrefererd == nil {
		return nodes, nil
	}
	for i := range inList {
		nodes.Append(&node.NodeScore{
			InsID:    inList[i].ID(),
			Score:    float64(selCtx.Affinity.NodePrefererd.Score(inList[i])),
			MvmNum:   inList[i].MvmNum,
			OrigNode: inList[i],
		})
	}

	return nodes, nil
}
