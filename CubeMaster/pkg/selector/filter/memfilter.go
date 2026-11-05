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

// memFilter rejects nodes whose free quota memory or physical free memory
// cannot fit the request.
type memFilter struct {
}

func NewMemFilter() *memFilter {
	return &memFilter{}
}

func (l *memFilter) ID() string {
	return constants.SelectorFilterID + "/" + "mem"
}

func (l *memFilter) String() string {
	return l.ID()
}

func (l *memFilter) Select(selCtx *selctx.SelectorCtx) (node.NodeList, error) {
	memq := selCtx.GetResMemFromCtx()
	sconf := config.GetConfig().Scheduler
	if memq == nil || sconf == nil {
		return nil, errors.New("memFilter: memq or sconf is nil")
	}
	inList := selCtx.Nodes()
	nodes := make(node.NodeList, 0, inList.Len())
	rejects := newRejectLog(selCtx)
	for i := range inList {
		quotaMemFree := inList[i].QuotaMem -
			sconf.EffectiveAllocated(inList[i].QuotaMemUsage)

		if quotaMemFree <= memq.Value()/1024/1024 {
			rejects.warnf("%v select:%v, quotaMemFree:%v, memq:%v",
				l.ID(), inList[i].ID(), quotaMemFree, memq.Value()/1024/1024)
			continue
		}

		loadMemFree := inList[i].MemMBTotal - inList[i].MemUsage
		nodeMaxMemReservedInMB := sconf.GetEffectiveNodeMaxMemReservedInMB(inList[i].InstanceType, inList[i].QuotaMem)

		if loadMemFree <= (memq.Value()/1024/1024 + nodeMaxMemReservedInMB) {
			rejects.warnf("%v select:%v, loadMemFree:%v, expectWithReserved:%v",
				l.ID(), inList[i].ID(), loadMemFree, memq.Value()/1024/1024+nodeMaxMemReservedInMB)
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
