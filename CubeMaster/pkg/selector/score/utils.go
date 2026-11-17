// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package score

import (
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
)

// getFactorWeightedAverageScore computes a node's weighted average score over
// the enabled factors: each factor yields a base score in [0,100] (higher
// means more idle / lower load), multiplied by its weight and summed. Base
// scores are clamped first: overcommit or anomalous metrics can push raw
// values out of range (quota usage >100% goes negative, create-limit raw
// values can exceed 100), and the profile pipeline enforces [0,100] on
// fail-closed plugins — an out-of-range score would fail the whole scheduling
// attempt.
func getFactorWeightedAverageScore(n *node.Node, enableWeightFactors []string) float64 {
	scores := float64(0)
	for _, v := range enableWeightFactors {
		switch v {
		case constants.WeightFactorCreateConcurrentLimit:
			scores += clampFactorScore(getCreateLimitScore(n)) * getFactorWeight(constants.WeightFactorCreateConcurrentLimit)
		case constants.WeightFactorMvmNum:
			scores += clampFactorScore(getMvmNumScore(n)) * getFactorWeight(constants.WeightFactorMvmNum)
		case constants.WeightFactorMetricUpdate:
			scores += clampFactorScore(getMetricUpdateDiff(n)) * getFactorWeight(constants.WeightFactorMetricUpdate)
		case constants.WeightFactorLocalMetricUpdate:
			scores += clampFactorScore(getMetricLocalUpdateDiff(n)) * getFactorWeight(constants.WeightFactorLocalMetricUpdate)
		case constants.WeightFactorQuotaCpu:
			scores += clampFactorScore(getQuotaCpuUsageScore(n)) * getFactorWeight(constants.WeightFactorQuotaCpu)
		case constants.WeightFactorQuotaMem:
			scores += clampFactorScore(getQuotaMemMbUsageScore(n)) * getFactorWeight(constants.WeightFactorQuotaMem)
		case constants.WeightFactorCpuUtil:
			scores += clampFactorScore(getCpuUtilScore(n)) * getFactorWeight(constants.WeightFactorCpuUtil)
		case constants.WeightFactorMemUsage:
			scores += clampFactorScore(getMemMbUsageScore(n)) * getFactorWeight(constants.WeightFactorMemUsage)
		case constants.WeightFactorCpuLoadUsage:
			scores += clampFactorScore(getCpuLoadUsageScore(n)) * getFactorWeight(constants.WeightFactorCpuLoadUsage)
		case constants.WeightFactorRealTimeCreateNum:
			scores += clampFactorScore(getRealTimeCreateNumScore(n)) * getFactorWeight(constants.WeightFactorRealTimeCreateNum)
		case constants.WeightFactorLocalCreateNum:
			scores += clampFactorScore(getLocalCreateNumScore(n)) * getFactorWeight(constants.WeightFactorLocalCreateNum)
		case constants.WeightFactorDataDiskUsage:
			scores += clampFactorScore(getDataDiskUsageScore(n)) * getFactorWeight(constants.WeightFactorDataDiskUsage)
		case constants.WeightFactorStorageDiskUsage:
			scores += clampFactorScore(getStorageUsageScore(n)) * getFactorWeight(constants.WeightFactorStorageDiskUsage)
		case constants.WeightFactorSysDiskUsage:
			scores += clampFactorScore(getSysDiskUsageScore(n)) * getFactorWeight(constants.WeightFactorSysDiskUsage)
		}
	}
	return scores
}

// clampFactorScore clamps a factor base score to the agreed [0,100] range.
func clampFactorScore(s float64) float64 {
	if s < 0 {
		return 0
	}
	if s > 100 {
		return 100
	}
	return s
}

// getReciprocal returns v/base in [0,1]; 0 when base is 0.
func getReciprocal(v int64, base int64) float64 {
	if base == 0 {
		return 0.0
	}

	f := float64(v) * 1.0 / float64(base)
	return f
}

func getFactorWeight(k string) float64 {
	sconf := config.GetConfig().Scheduler
	if sconf == nil || sconf.Score == nil {
		return 0.0
	}
	v, ok := sconf.Score.ResourceWeights[k]
	if !ok {
		return 0.0
	}
	return v
}

func getMetricUpdateDiff(n *node.Node) float64 {
	return getReciprocal(n.MetricUpdate.Unix(), time.Now().Unix())
}

func getMetricLocalUpdateDiff(n *node.Node) float64 {
	return getReciprocal(n.MetricUpdate.Unix(), time.Now().Unix())
}

func getCreateLimitScore(n *node.Node) float64 {
	return float64(n.CreateConcurrentNum)
}

func getRealTimeCreateNumScore(n *node.Node) float64 {
	f := getReciprocal(n.RealTimeCreateNum, n.CreateConcurrentNum)
	return 100.0 - f*100.0
}

func getLocalCreateNumScore(n *node.Node) float64 {
	max := n.CreateConcurrentNum
	localcnt := localcache.LocalCreateConcurrentLimit(n)

	gotGlobalLocalcnt := localcnt * localcache.HealthyMasterNodes()
	f := getReciprocal(gotGlobalLocalcnt, max)
	return 100.0 - f*100.0
}

func getMvmNumScore(n *node.Node) float64 {
	max := localcache.MaxMvmLimit(n)
	f := getReciprocal(n.MvmNum, max)
	return 100.0 - f*100.0
}

func getQuotaCpuUsageScore(n *node.Node) float64 {
	sconf := config.GetConfig().Scheduler
	if sconf == nil {
		return 0.0
	}
	if n.QuotaCpu <= 0 {

		return 0.0
	}
	f := getReciprocal(sconf.EffectiveAllocated(n.QuotaCpuUsage), n.QuotaCpu)
	return 100.0 - f*100.0
}

func getQuotaMemMbUsageScore(n *node.Node) float64 {
	sconf := config.GetConfig().Scheduler
	if sconf == nil {
		return 0.0
	}
	if n.QuotaMem <= 0 {
		return 0.0
	}
	f := getReciprocal(sconf.EffectiveAllocated(n.QuotaMemUsage), n.QuotaMem)
	return 100.0 - f*100.0
}

func getCpuLoadUsageScore(n *node.Node) float64 {
	if n.CpuTotal <= 0 {

		return 0.0
	}
	f := n.CpuLoadUsage / float64(n.CpuTotal)
	return 100.0 - f*100.0
}

func getCpuUtilScore(n *node.Node) float64 {
	return 100.0 - n.CpuUtil
}

func getMemMbUsageScore(n *node.Node) float64 {
	if n.MemMBTotal <= 0 {

		return 0.0
	}
	f := getReciprocal(n.MemUsage, n.MemMBTotal)
	return 100.0 - f*100.0
}

func getDataDiskUsageScore(n *node.Node) float64 {
	return 100.0 - n.DataDiskUsagePer
}

func getSysDiskUsageScore(n *node.Node) float64 {
	return 100.0 - n.SysDiskUsagePer
}

func getStorageUsageScore(n *node.Node) float64 {
	return 100.0 - n.StorageDiskUsagePer
}
