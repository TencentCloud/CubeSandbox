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
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

const binpackScoreName = "binpack_score"

type binpackScore struct{}

func NewBinpackScore() *binpackScore {
	return &binpackScore{}
}

func (l *binpackScore) ID() string {
	return constants.SelectorScoreID + "/" + binpackScoreName
}

func (l *binpackScore) String() string {
	return l.ID()
}

func (l *binpackScore) Weight() float64 {
	weight, disable := binpackScoreRuntime()
	if disable {
		return 0
	}
	return weight
}

func (l *binpackScore) Disable() bool {
	_, disable := binpackScoreRuntime()
	return disable
}

func (l *binpackScore) Select(selCtx *selctx.SelectorCtx) (nodes node.NodeScoreList, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = ret.Errorf(errorcode.ErrorCode_MasterInternalError, "binpackScore panic:%s", r)
		}
	}()

	if l.Disable() {
		return nil, nil
	}

	inList := selCtx.Nodes()
	nodes = make(node.NodeScoreList, 0, inList.Len())
	cpuW, memW, mvmW := binpackScoreFactorWeights()
	for i := range inList {
		n := inList[i]
		if n == nil {
			continue
		}
		nodes.Append(&node.NodeScore{
			InsID:    n.ID(),
			Score:    binpackOccupancyScore(n, cpuW, memW, mvmW),
			MvmNum:   n.MvmNum,
			OrigNode: n,
		})
	}
	return nodes, nil
}

func binpackScoreRuntime() (weight float64, disable bool) {
	cfg := getBinpackScoreConfig()
	if cfg == nil {
		return 1, false
	}
	if cfg.Disable {
		return 0, true
	}
	if cfg.Weight == 0 {
		return 0, true
	}
	// Negative weights are rejected at config load (validateBinpackScoreWeight).
	return cfg.Weight, false
}

func binpackScoreFactorWeights() (cpuW, memW, mvmW float64) {
	cfg := getBinpackScoreConfig()
	cpuW, memW, mvmW = 1, 1, 1
	if cfg == nil {
		return cpuW, memW, mvmW
	}
	if cfg.CPUWeight > 0 {
		cpuW = cfg.CPUWeight
	}
	if cfg.MemWeight > 0 {
		memW = cfg.MemWeight
	}
	if cfg.MvmWeight > 0 {
		mvmW = cfg.MvmWeight
	}
	return cpuW, memW, mvmW
}

func getBinpackScoreConfig() *config.BinpackScore {
	cfg := config.GetConfig()
	if cfg == nil || cfg.Scheduler == nil || cfg.Scheduler.Score == nil {
		return nil
	}
	return cfg.Scheduler.Score.ScorePluginConf.BinpackScore
}

func binpackOccupancyScore(n *node.Node, cpuW, memW, mvmW float64) float64 {
	var weighted float64
	var totalW float64

	var sconf *config.SchedulerConf
	if cfg := config.GetConfig(); cfg != nil && cfg.Scheduler != nil {
		sconf = &cfg.Scheduler.SchedulerConf
	}

	if cpuW > 0 {
		cpuUtil := occupancyRatio(allocatedCPU(sconf, n), n.QuotaCpu)
		weighted += cpuUtil * cpuW
		totalW += cpuW
	}
	if memW > 0 {
		memUtil := occupancyRatio(allocatedMem(sconf, n), n.QuotaMem)
		weighted += memUtil * memW
		totalW += memW
	}
	if mvmW > 0 {
		// Use the same authoritative per-node capacity helper as other
		// scorers (falls back to instance-type / global NodeMaxMvmNum).
		mvmUtil := occupancyRatio(n.MvmNum, localcache.MaxMvmLimit(n))
		weighted += mvmUtil * mvmW
		totalW += mvmW
	}
	if totalW == 0 {
		return 0
	}
	return 100 * weighted / totalW
}

func allocatedCPU(sconf *config.SchedulerConf, n *node.Node) int64 {
	if sconf == nil {
		return n.QuotaCpuUsage
	}
	return sconf.EffectiveAllocated(n.QuotaCpuUsage)
}

func allocatedMem(sconf *config.SchedulerConf, n *node.Node) int64 {
	if sconf == nil {
		return n.QuotaMemUsage
	}
	return sconf.EffectiveAllocated(n.QuotaMemUsage)
}

func occupancyRatio(used, cap int64) float64 {
	if cap <= 0 {
		return 0
	}
	ratio := float64(used) / float64(cap)
	if ratio < 0 {
		return 0
	}
	if ratio > 1 {
		return 1
	}
	return ratio
}
