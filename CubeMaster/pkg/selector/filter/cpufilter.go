// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package filter

import (
	"errors"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

// cpuFilter rejects nodes with insufficient CPU quota or excessive CPU utilization.
type cpuFilter struct {
}

func NewCpuFilter() *cpuFilter {
	return &cpuFilter{}
}

func (l *cpuFilter) ID() string {
	return constants.SelectorFilterID + "/" + "cpu"
}

func (l *cpuFilter) String() string {
	return l.ID()
}

func (l *cpuFilter) Select(selCtx *selctx.SelectorCtx) (node.NodeList, error) {
	cpuq := selCtx.GetResCpuFromCtx()
	sconf := config.GetConfig().Scheduler
	if cpuq == nil || sconf == nil {
		return nil, errors.New("cpuq or sconf is nil")
	}

	inList := selCtx.Nodes()
	nodes := make(node.NodeList, 0, inList.Len())
	rejects := newRejectLog(selCtx)
	for i := range inList {

		quotaCpuFree := inList[i].QuotaCpu -
			sconf.EffectiveAllocated(inList[i].QuotaCpuUsage)
		if quotaCpuFree <= cpuq.MilliValue() {
			rejects.warnf("%v select:%v, quotaCpuFree:%v, cpuq:%v",
				l.ID(), inList[i].ID(), quotaCpuFree, cpuq.MilliValue())
			continue
		}

		if inList[i].CpuUtil >= sconf.NodeMaxCpuUtil {
			rejects.warnf("%v select:%v, CpuUtil:%v, NodeMaxCpuUsage:%v",
				l.ID(), inList[i].ID(), inList[i].CpuUtil, sconf.NodeMaxCpuUtil)
			continue
		}
		nodes.Append(inList[i])
	}
	rejects.flush(l.ID())
	if log.IsDebug() {
		log.G(selCtx.Ctx).Debugf("%v select:%v", l.ID(), nodes.String())
	}
	return nodes, nil
}
