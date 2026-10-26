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

type thirtpartyFilter struct {
}

func NewThirtpartyFilter() *thirtpartyFilter {
	return &thirtpartyFilter{}
}

func (l *thirtpartyFilter) ID() string {
	return constants.SelectorFilterID + "/" + "thirtparty"
}

func (l *thirtpartyFilter) String() string {
	return l.ID()
}

func (l *thirtpartyFilter) Select(selCtx *selctx.SelectorCtx) (node.NodeList, error) {
	res := selCtx.GetReqRes()
	sconf := config.GetConfig().Scheduler
	if res == nil || sconf == nil {
		return nil, errors.New("thirtpartyFilter: reqres or sconf is nil")
	}
	if res.EnableSlowPath {
		return selCtx.Nodes(), nil
	}
	if sconf.ThirtpartyFilterInstanceType == nil {

		return selCtx.Nodes(), nil
	}

	if _, ok := sconf.ThirtpartyFilterInstanceType[selCtx.InstanceType]; !ok {

		return selCtx.Nodes(), nil
	}

	nodes := selCtx.Nodes()
	if log.IsDebug() {
		log.G(selCtx.Ctx).Debugf("%v select:%v", l.ID(), nodes.String())
	} else {
		log.G(selCtx.Ctx).Infof("%v select_size:%v", l.ID(), nodes.Len())
	}
	return nodes, nil
}
