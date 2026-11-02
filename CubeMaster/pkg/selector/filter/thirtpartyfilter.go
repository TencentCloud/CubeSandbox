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

// thirtpartyFilter 第三方过滤插件（占位实现）：
// 当前仅对特定实例类型且非慢路径的请求生效，且不做实际过滤，直接返回原节点列表
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

// Select 当前实现不剔除任何节点：
// 慢路径请求、未配置实例类型过滤表、或当前实例类型不在过滤表内时直接返回原列表
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
