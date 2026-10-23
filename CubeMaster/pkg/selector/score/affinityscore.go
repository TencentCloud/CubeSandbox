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

// affinityScore 节点亲和性偏好评分插件：
// 依据软性亲和性条款（PreferredSchedulingTerms）为节点打分，作为调度偏好的软约束
type affinityScore struct {
	weight float64
}

func affinityScoreConf() *config.AffinityScore {
	sched := config.GetConfig().Scheduler
	if sched == nil || sched.Score == nil {
		return nil
	}
	return sched.Score.ScorePluginConf.AffinityScore
}

// NewAffinityScore tolerates a missing legacy plugin_conf block: the scorer
// then has no weight of its own and a profile entry must carry one. Scoring
// itself only depends on the request's affinity terms, not on global config.
func NewAffinityScore() *affinityScore {
	conf := affinityScoreConf()
	if conf == nil {
		CubeLog.Warnf("scheduler.score.plugin_conf.affinity_score is not configured; affinity_score needs an explicit profile weight to take effect")
		return &affinityScore{}
	}
	return &affinityScore{
		weight: conf.Weight,
	}
}

func (l *affinityScore) ID() string {
	return constants.SelectorScoreID + "/" + "affinity_score"
}

func (l *affinityScore) String() string {
	return l.ID()
}

func (l *affinityScore) Weight() float64 {
	return l.weight
}

func (l *affinityScore) Disable() bool {
	conf := affinityScoreConf()
	return conf == nil || conf.Disable
}

// Select 为每个候选节点计算亲和性偏好得分；未配置偏好条款时全部为 0 分
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
