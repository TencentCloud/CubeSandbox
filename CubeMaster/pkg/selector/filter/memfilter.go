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
	for i := range inList {
		quotaMemFree := sconf.EffectiveQuotaMem(inList[i].InstanceType, inList[i].QuotaMem) -
			sconf.EffectiveAllocated(inList[i].QuotaMemUsage)

		if quotaMemFree <= memq.Value()/1024/1024 {
			log.G(selCtx.Ctx).Warnf("%v select:%v, quotaMemFree:%v, memq:%v",
				l.ID(), inList[i].ID(), quotaMemFree, memq.Value()/1024/1024)
			continue
		}

		loadMemFree := inList[i].MemMBTotal - inList[i].MemUsage
		nodeMaxMemReservedInMB := sconf.GetEffectiveNodeMaxMemReservedInMB(inList[i].InstanceType, inList[i].QuotaMem)

		if loadMemFree <= (memq.Value()/1024/1024 + nodeMaxMemReservedInMB) {
			log.G(selCtx.Ctx).Warnf("%v select:%v, loadMemFree:%v, expectWithReserved:%v",
				l.ID(), inList[i].ID(), loadMemFree, memq.Value()/1024/1024+nodeMaxMemReservedInMB)
			continue
		}
		nodes.Append(inList[i])
	}
	if log.IsDebug() {
		log.G(selCtx.Ctx).Debugf("%v select:%v", l.ID(), nodes.String())
	} else {
		log.G(selCtx.Ctx).Infof("%v select_size:%v", l.ID(), nodes.Len())
	}
	return nodes, nil
}
