// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package scheduler provides a scheduler for the cube-master
package scheduler

import (
	"context"
	"errors"
	"sync"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/backofffilter"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/filter"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/postscore"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/prefilter"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/score"
)

var (
	ErrPreSelect = errors.New("PreSelector")
	ErrNoRes     = errors.New("no more resource")
)

var scheduler = struct {
	sync.RWMutex
	filter          []filter.Selector
	score           []score.Selector
	postScore       postscore.Selector
	preSelector     filter.Selector
	backoffSelector filter.Selector
}{filter: make([]filter.Selector, 0)}

func InitScheduler(ctx context.Context) {
	scheduler.preSelector = prefilter.NewPreFilter()
	scheduler.backoffSelector = backofffilter.NewBackoffFilter()
	scheduler.filter = filter.NewSelector()
	scheduler.score = score.NewSelector(ctx)
	scheduler.postScore = postscore.NewSelector()

	initTask(ctx)
}
