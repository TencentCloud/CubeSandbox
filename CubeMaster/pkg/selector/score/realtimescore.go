// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package score

import (
	"errors"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/ret"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
	"k8s.io/apimachinery/pkg/api/resource"
)

type realTimeWeightedAverageScore struct {
	weight float64
}

func NewRealTimeWeightedAverageScore() *realTimeWeightedAverageScore {
	if config.GetConfig().Scheduler.Score.ScorePluginConf.RealTimeWeightedAverage == nil {
		panic("config.Scheduler.Score.ScorePluginConf.RealTimeWeightedAverage is nil")
	}
	return &realTimeWeightedAverageScore{
		weight: config.GetConfig().Scheduler.Score.ScorePluginConf.RealTimeWeightedAverage.Weight,
	}
}

func (l *realTimeWeightedAverageScore) ID() string {
	return constants.SelectorScoreID + "/" + "real_time_weighted_average"
}

func (l *realTimeWeightedAverageScore) String() string {
	return l.ID()
}

func (l *realTimeWeightedAverageScore) Weight() float64 {
	return l.weight
}
func (l *realTimeWeightedAverageScore) Disable() bool {
	return config.GetConfig().Scheduler.Score.ScorePluginConf.RealTimeWeightedAverage.Disable
}

func (l *realTimeWeightedAverageScore) Select(selCtx *selctx.SelectorCtx) (nodes node.NodeScoreList,
	err error) {
	defer func() {
		if r := recover(); r != nil {
			err = ret.Errorf(errorcode.ErrorCode_MasterInternalError, "realTimeWeightedAverageScore panic:%s", r)
		}
	}()

	cpuq := selCtx.GetResCpuFromCtx()
	memq := selCtx.GetResMemFromCtx()
	if cpuq == nil || memq == nil {
		return nil, ret.Errorf(errorcode.ErrorCode_MasterInternalError,
			"cpu request or mem request is nil")
	}
	sconf := config.GetConfig().Scheduler
	if sconf == nil || sconf.Score == nil || sconf.Score.ScorePluginConf.RealTimeWeightedAverage == nil ||
		sconf.Score.ResourceWeights == nil {
		return nil, nil
	}
	if l.Disable() {
		return nil, nil
	}

	inList := selCtx.Nodes()
	nodes = make(node.NodeScoreList, 0, inList.Len())

	totalWeight, err := getRealTimeTotalWeight()
	if err != nil || totalWeight == 0 {
		return nil, nil
	}

	for i := range inList {
		nscore := getRealtimeWeightedAverageScore(inList[i], cpuq, memq) / totalWeight
		nodes.Append(&node.NodeScore{
			InsID:    inList[i].ID(),
			Score:    nscore,
			MvmNum:   inList[i].MvmNum,
			OrigNode: inList[i],
		})
	}

	return nodes, nil
}

func getRealTimeTotalWeight() (float64, error) {
	schedConf := config.GetConfig().Scheduler
	if schedConf == nil || schedConf.Score == nil {
		return 0, errors.New("scheduler score conf is nil")
	}
	sconf := schedConf.Score.ScorePluginConf.RealTimeWeightedAverage
	if sconf == nil {
		return 0, errors.New("RealTime conf is nil")
	}
	w := float64(0)
	for _, v := range sconf.EnableWeightFactors {
		w += getFactorWeight(v)
	}
	return w, nil
}

func getRealtimeWeightedAverageScore(n *node.Node, cpuq, memq *resource.Quantity) float64 {
	schedConf := config.GetConfig().Scheduler
	if schedConf == nil || schedConf.Score == nil {
		return 0
	}
	sconf := schedConf.Score.ScorePluginConf.RealTimeWeightedAverage
	if sconf == nil {
		return 0
	}

	scores := getFactorWeightedAverageScore(n, sconf.EnableWeightFactors)

	if cpuq != nil {

		cpuReqValue := cpuq.MilliValue()
		effCpu := schedConf.EffectiveQuotaCpu(n.InstanceType, n.QuotaCpu)
		cpuLeft := getReciprocal(effCpu-schedConf.EffectiveAllocated(n.QuotaCpuUsage)-cpuReqValue, effCpu)
		scores += cpuLeft * getFactorWeight(constants.WeightFactorReqCpu)
	}

	if memq != nil {

		memReqValue := memq.Value() / 1024 / 1024
		effMem := schedConf.EffectiveQuotaMem(n.InstanceType, n.QuotaMem)
		memLeft := getReciprocal(effMem-schedConf.EffectiveAllocated(n.QuotaMemUsage)-memReqValue, effMem)
		scores += memLeft * getFactorWeight(constants.WeightFactorReqMem)
	}
	return scores
}
