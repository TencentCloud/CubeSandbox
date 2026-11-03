// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package postscore

import (
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

// Selector is the post-score plugin interface: it adjusts node scores after
// scoring completes.
type Selector interface {
	PostedScore(selCtx *selctx.SelectorCtx, result map[string]*node.NodeScore) error

	ID() string

	Disable() bool
}

// NewSelector builds the post-score plugins; nil when PostScore is not configured.
func NewSelector() Selector {
	conf := config.GetConfig().Scheduler
	if conf == nil || conf.PostScore == nil {
		return nil
	}
	return &whilelistWeightedScore{}
}
