// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package postscore

import (
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

type Selector interface {
	PostedScore(selCtx *selctx.SelectorCtx, result map[string]*node.NodeScore) error

	ID() string

	Disable() bool
}

func NewSelector() Selector {
	conf := config.GetConfig().Scheduler
	if conf == nil || conf.PostScore == nil {
		return nil
	}
	return &whilelistWeightedScore{}
}
