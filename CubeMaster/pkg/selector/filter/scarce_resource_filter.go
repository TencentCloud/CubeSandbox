// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package filter

import (
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/scarceresource"
)

type scarceResourceFilter struct{}

func NewScarceResourceFilter() *scarceResourceFilter {
	return &scarceResourceFilter{}
}

func (l *scarceResourceFilter) ID() string {
	return constants.SelectorFilterID + "/" + "scarce_resource"
}

func (l *scarceResourceFilter) String() string {
	return l.ID()
}

func (l *scarceResourceFilter) Select(selCtx *selctx.SelectorCtx) (node.NodeList, error) {
	inList := selCtx.Nodes()
	nodes := scarceresource.FilterNodes(selCtx, inList, l.ID(), nil)
	if log.IsDebug() {
		log.G(selCtx.Ctx).Debugf("%v select:%v", l.ID(), nodes.String())
	} else {
		log.G(selCtx.Ctx).Infof("%v select_size:%v", l.ID(), nodes.Len())
	}
	return nodes, nil
}
