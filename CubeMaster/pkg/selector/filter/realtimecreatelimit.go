// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package filter

import (
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

// realtimecreatelimit rejects nodes whose current create concurrency has
// reached the limit, keeping create pressure off single nodes.
type realtimecreatelimit struct {
}

func NewRealtimecreatelimit() *realtimecreatelimit {
	return &realtimecreatelimit{}
}

func (l *realtimecreatelimit) ID() string {
	return constants.SelectorFilterID + "/" + "realtime_create_num"
}

func (l *realtimecreatelimit) String() string {
	return l.ID()
}

// Select folds the node-local create concurrency into a global estimate via
// the master node count before checking the limit.
func (l *realtimecreatelimit) Select(selCtx *selctx.SelectorCtx) (node.NodeList, error) {
	inList := selCtx.Nodes()
	nodes := make(node.NodeList, 0, inList.Len())
	rejects := newRejectLog(selCtx)
	for i := range inList {

		realGlobalcnt := localcache.RealTimeCreateConcurrentLimit(inList[i])
		limitcnt := localcache.CreateConcurrentLimit(inList[i])
		if realGlobalcnt >= limitcnt {
			rejects.errorf("%s RealTimeCreateConcurrentLimit exceed:%d", inList[i].IP, realGlobalcnt)
			continue
		}

		localcnt := localcache.LocalCreateConcurrentLimit(inList[i])
		gotGlobalLocalcnt := localcnt * localcache.HealthyMasterNodes()
		if gotGlobalLocalcnt >= limitcnt {
			rejects.errorf("%s LocalCreateConcurrentLimit exceed,localcnt:%d,gotGlobalLocalcnt:%d",
				inList[i].IP, localcnt, gotGlobalLocalcnt)
			continue
		}

		nodes.Append(inList[i])
	}
	rejects.flush(l.ID())
	if log.IsDebug() {
		log.G(selCtx.Ctx).Debugf("%v select:%v", l.ID(), nodes.String())
	}
	// When every candidate is dropped by the create-concurrency limit, stamp a
	// reject reason so the subsequent no_node failure is classified as
	// concurrency_limit in metrics instead of a bare no_node. Partial rejects
	// are normal filtering and are not stamped.
	if inList.Len() > 0 && nodes.Len() == 0 {
		selCtx.SetRejectReason(selctx.RejectReasonConcurrencyLimit)
	}
	return nodes, nil
}
