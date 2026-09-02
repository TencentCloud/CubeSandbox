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

// getFactorWeightedAverageScore 按启用的因子列表计算节点加权平均分：
// 各因子基础分方向一致（越高越好：资源越空闲/负载越低/指标越新鲜），再乘以对应权重求和。
// 注意各因子量纲并不统一：多数因子落在 0~100 区间；getMetricUpdateDiff 与
// getMetricLocalUpdateDiff 返回 ≈[0,1] 的 Unix 时间比值（现实陈旧度下几乎恒为 1）；
// getCreateLimitScore 直接返回原始并发上限值——调整因子权重时须考虑量纲差异
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

// getReciprocal 计算 v 占 base 的比例（0~1），base 为 0 时返回 0
func getReciprocal(v int64, base int64) float64 {
	if base == 0 {
		return 0.0
	}

	f := float64(v) * 1.0 / float64(base)
	return f
}

// getFactorWeight 取配置中某权重因子的权重值
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

// getMetricUpdateDiff 节点指标更新时间越新得分越高（防止陈旧指标节点被选中）
func getMetricUpdateDiff(n *node.Node) float64 {
	return getReciprocal(n.MetricUpdate.Unix(), time.Now().Unix())
}

// getMetricLocalUpdateDiff 当前实现与 getMetricUpdateDiff 完全相同：读的同样是
// n.MetricUpdate（全局指标更新时间），而非名字暗示的 n.MetricLocalUpdateAt，
// 因此本地指标新鲜度实际不参与该因子得分。这是疑似复制遗留的缺陷（应读
// n.MetricLocalUpdateAt）；修正它会改变线上打分行为，将由独立的行为修复 PR 处理，
// 本 PR 仅记录实际行为，函数体保持不变。
func getMetricLocalUpdateDiff(n *node.Node) float64 {
	return getReciprocal(n.MetricUpdate.Unix(), time.Now().Unix())
}

// getCreateLimitScore 节点创建并发上限越高得分越高
func getCreateLimitScore(n *node.Node) float64 {
	return float64(n.CreateConcurrentNum)
}

// getRealTimeCreateNumScore 节点实时创建并发数占比越低得分越高
func getRealTimeCreateNumScore(n *node.Node) float64 {
	f := getReciprocal(n.RealTimeCreateNum, n.CreateConcurrentNum)
	return 100.0 - f*100.0
}

// getLocalCreateNumScore 节点本地创建并发数折算全局后占比越低得分越高
func getLocalCreateNumScore(n *node.Node) float64 {
	max := n.CreateConcurrentNum
	localcnt := localcache.LocalCreateConcurrentLimit(n)

	gotGlobalLocalcnt := localcnt * localcache.HealthyMasterNodes()
	f := getReciprocal(gotGlobalLocalcnt, max)
	return 100.0 - f*100.0
}

// getMvmNumScore 节点 MVM 数量占上限比例越低得分越高
func getMvmNumScore(n *node.Node) float64 {
	max := localcache.MaxMvmLimit(n)
	f := getReciprocal(n.MvmNum, max)
	return 100.0 - f*100.0
}

// getQuotaCpuUsageScore 节点 CPU 配额使用率越低得分越高
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

// getQuotaMemMbUsageScore 节点内存配额使用率越低得分越高
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

// getCpuLoadUsageScore 节点 CPU 负载占比越低得分越高
func getCpuLoadUsageScore(n *node.Node) float64 {
	if n.CpuTotal <= 0 {

		return 0.0
	}
	f := n.CpuLoadUsage / float64(n.CpuTotal)
	return 100.0 - f*100.0
}

// getCpuUtilScore 节点 CPU 利用率越低得分越高
func getCpuUtilScore(n *node.Node) float64 {
	return 100.0 - n.CpuUtil
}

// getMemMbUsageScore 节点内存使用占比越低得分越高
func getMemMbUsageScore(n *node.Node) float64 {
	if n.MemMBTotal <= 0 {

		return 0.0
	}
	f := getReciprocal(n.MemUsage, n.MemMBTotal)
	return 100.0 - f*100.0
}

// getDataDiskUsageScore 节点数据盘使用率越低得分越高
func getDataDiskUsageScore(n *node.Node) float64 {
	return 100.0 - n.DataDiskUsagePer
}

// getSysDiskUsageScore 节点系统盘使用率越低得分越高
func getSysDiskUsageScore(n *node.Node) float64 {
	return 100.0 - n.SysDiskUsagePer
}

// getStorageUsageScore 节点存储盘使用率越低得分越高
func getStorageUsageScore(n *node.Node) float64 {
	return 100.0 - n.StorageDiskUsagePer
}
