// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

// trackTemplateCreate 在 Cubelet 创建调用期间登记 (node, template) 在途创建计数，
// 返回的函数负责在调用结束后归还计数。该计数是 template_reuse 策略
// template_local_pressure 打分插件的输入，用于把同模板创建压力在副本节点间分散。
// 请求不带模板 ID 时返回空操作，不留下任何计数。
func trackTemplateCreate(host *node.Node, selCtx *selctx.SelectorCtx) func() {
	if host == nil || selCtx == nil || selCtx.ReqRes == nil {
		return func() {}
	}
	templateID := selCtx.ReqRes.TemplateID
	if templateID == "" {
		return func() {}
	}
	localcache.IncrNodeTemplateCreate(host.ID(), templateID)
	return func() {
		localcache.DecrNodeTemplateCreate(host.ID(), templateID)
	}
}
