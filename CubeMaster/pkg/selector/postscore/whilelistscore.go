// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package postscore

import (
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/ret"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

// whilelistWeightedScore 评分后处理插件：基于活跃/负面白名单对节点分数做加权调整。
// 活跃白名单中的节点加分（提升被选中的概率），负面白名单中的节点按比例扣分
type whilelistWeightedScore struct {
}

func (l *whilelistWeightedScore) ID() string {
	return constants.SelectorPostScoreID + "/" + "whilelist_weighted_score"
}

func (l *whilelistWeightedScore) String() string {
	return l.ID()
}

func (l *whilelistWeightedScore) Disable() bool {
	return config.GetConfig().Scheduler.PostScore.Disable
}

// PostedScore 在评分完成后调整各节点分数：
// 1. 活跃白名单：节点分数 + 平均分 * 活跃白名单权重因子；
// 2. 负面白名单：节点分数 - 自身分数 * 负面白名单权重因子（按比例扣分）
func (l *whilelistWeightedScore) PostedScore(selCtx *selctx.SelectorCtx, result map[string]*node.NodeScore) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = ret.Errorf(errorcode.ErrorCode_MasterInternalError, "PostedScore panic:%s", r)
		}
	}()

	if config.GetConfig().Scheduler.PostScore.Disable {
		return nil
	}
	sconf := config.GetConfig().Scheduler.PostScore
	if sconf == nil {
		return nil
	}
	if len(result) == 0 {
		return nil
	}

	if len(sconf.ActiveWhiteListMap) > 0 {
		var sum float64
		for _, v := range result {
			sum += v.Score
		}
		mean := sum / float64(len(result))
		for k := range sconf.ActiveWhiteListMap {
			if v, ok := result[k]; ok {
				v.Score = v.Score + mean*getFactorWeight(constants.WeightFactorActiveWhiteList)
			}
		}
	}

	for k := range sconf.NegativeWhiteListMap {
		if v, ok := result[k]; ok {
			v.Score = v.Score - v.Score*getFactorWeight(constants.WeightFactorNegativeWhiteList)
		}
	}
	return nil
}

// getFactorWeight 取配置中某权重因子经 ParamFactor 放大后的权重值
func getFactorWeight(k string) float64 {
	sconf := config.GetConfig().Scheduler.PostScore
	if sconf == nil {
		return 0.0
	}
	v, ok := sconf.ResourceWeights[k]
	if !ok {
		return 0.0
	}
	return v * sconf.ParamFactor
}
