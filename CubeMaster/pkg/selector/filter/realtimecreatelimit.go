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

func (l *realtimecreatelimit) Select(selCtx *selctx.SelectorCtx) (node.NodeList, error) {
	inList := selCtx.Nodes()
	nodes := make(node.NodeList, 0, inList.Len())
	for i := range inList {

		realGlobalcnt := localcache.RealTimeCreateConcurrentLimit(inList[i])
		limitcnt := localcache.CreateConcurrentLimit(inList[i])
		if realGlobalcnt >= limitcnt {
			log.G(selCtx.Ctx).Errorf("%s RealTimeCreateConcurrentLimit exceed:%d", inList[i].IP, realGlobalcnt)
			continue
		}

		localcnt := localcache.LocalCreateConcurrentLimit(inList[i])
		gotGlobalLocalcnt := localcnt * localcache.HealthyMasterNodes()
		if gotGlobalLocalcnt >= limitcnt {
			log.G(selCtx.Ctx).Errorf("%s LocalCreateConcurrentLimit exceed,localcnt:%d,gotGlobalLocalcnt:%d",
				inList[i].IP, localcnt, gotGlobalLocalcnt)
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
