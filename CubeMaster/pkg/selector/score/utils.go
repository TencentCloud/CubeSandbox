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

func getFactorWeightedAverageScore(n *node.Node, enableWeightFactors []string) float64 {
	scores := float64(0)
	for _, v := range enableWeightFactors {
		switch v {
		case constants.WeightFactorCreateConcurrentLimit:
			scores += getCreateLimitScore(n) * getFactorWeight(constants.WeightFactorCreateConcurrentLimit)
		case constants.WeightFactorMvmNum:
			scores += getMvmNumScore(n) * getFactorWeight(constants.WeightFactorMvmNum)
		case constants.WeightFactorMetricUpdate:
			scores += getMetricUpdateDiff(n) * getFactorWeight(constants.WeightFactorMetricUpdate)
		case constants.WeightFactorLocalMetricUpdate:
			scores += getMetricLocalUpdateDiff(n) * getFactorWeight(constants.WeightFactorLocalMetricUpdate)
		case constants.WeightFactorQuotaCpu:
			scores += getQuotaCpuUsageScore(n) * getFactorWeight(constants.WeightFactorQuotaCpu)
		case constants.WeightFactorQuotaMem:
			scores += getQuotaMemMbUsageScore(n) * getFactorWeight(constants.WeightFactorQuotaMem)
		case constants.WeightFactorCpuUtil:
			scores += getCpuUtilScore(n) * getFactorWeight(constants.WeightFactorCpuUtil)
		case constants.WeightFactorMemUsage:
			scores += getMemMbUsageScore(n) * getFactorWeight(constants.WeightFactorMemUsage)
		case constants.WeightFactorCpuLoadUsage:
			scores += getCpuLoadUsageScore(n) * getFactorWeight(constants.WeightFactorCpuLoadUsage)
		case constants.WeightFactorRealTimeCreateNum:
			scores += getRealTimeCreateNumScore(n) * getFactorWeight(constants.WeightFactorRealTimeCreateNum)
		case constants.WeightFactorLocalCreateNum:
			scores += getLocalCreateNumScore(n) * getFactorWeight(constants.WeightFactorLocalCreateNum)
		case constants.WeightFactorDataDiskUsage:
			scores += getDataDiskUsageScore(n) * getFactorWeight(constants.WeightFactorDataDiskUsage)
		case constants.WeightFactorStorageDiskUsage:
			scores += getStorageUsageScore(n) * getFactorWeight(constants.WeightFactorStorageDiskUsage)
		case constants.WeightFactorSysDiskUsage:
			scores += getSysDiskUsageScore(n) * getFactorWeight(constants.WeightFactorSysDiskUsage)
		}
	}
	return scores
}

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
	effCpu := sconf.EffectiveQuotaCpu(n.InstanceType, n.QuotaCpu)
	if effCpu <= 0 {

		return 0.0
	}
	f := getReciprocal(sconf.EffectiveAllocated(n.QuotaCpuUsage), effCpu)
	return 100.0 - f*100.0
}

func getQuotaMemMbUsageScore(n *node.Node) float64 {
	sconf := config.GetConfig().Scheduler
	if sconf == nil {
		return 0.0
	}
	effMem := sconf.EffectiveQuotaMem(n.InstanceType, n.QuotaMem)
	if effMem <= 0 {
		return 0.0
	}
	f := getReciprocal(sconf.EffectiveAllocated(n.QuotaMemUsage), effMem)
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
