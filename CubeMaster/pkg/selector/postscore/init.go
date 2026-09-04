// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package postscore

import (
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

// Selector 评分后处理插件接口：在评分完成后对节点分数进行调整
type Selector interface {
	PostedScore(selCtx *selctx.SelectorCtx, result map[string]*node.NodeScore) error

	ID() string

	Disable() bool
}

// NewSelector 创建评分后处理插件；未配置 PostScore 时返回 nil
func NewSelector() Selector {
	conf := config.GetConfig().Scheduler
	if conf == nil || conf.PostScore == nil {
		return nil
	}
	return &whilelistWeightedScore{}
}
