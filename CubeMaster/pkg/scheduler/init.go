// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package scheduler provides a scheduler for the cube-master
package scheduler

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/profile"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/backofffilter"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/filter"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/plugin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/plugin/expr"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/plugin/grpcplugin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/postscore"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/prefilter"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/score"
)

var (
	// ErrPreSelect 预选择阶段失败（prefilter / backoff 选择器执行出错）
	ErrPreSelect = errors.New("PreSelector")
	// ErrNoRes 没有可用节点资源
	ErrNoRes = errors.New("no more resource")
)

// scheduler 全局调度器状态，按职责保存各阶段的选择器实例：
// filter：主过滤阶段（并行执行多个过滤插件）
// score：评分阶段（多个评分插件）
// postScore：评分后处理（如白名单加权调整）
// preSelector：预过滤（先取可调度节点再筛选）
// backoffSelector：兜底过滤（主过滤失败时放宽条件的备选路径）
var scheduler = struct {
	sync.RWMutex
	filter          []filter.Selector
	score           []score.Selector
	postScore       postscore.Selector
	preSelector     filter.Selector
	backoffSelector filter.Selector
	registry        *plugin.Registry
	profiles        atomic.Pointer[profile.Set]
}{filter: make([]filter.Selector, 0)}

// InitScheduler 初始化调度器的各个阶段插件，并启动调度任务（缓冲队列与并发限制监控）
func InitScheduler(ctx context.Context) error {
	scheduler.preSelector = prefilter.NewPreFilter()
	scheduler.backoffSelector = backofffilter.NewBackoffFilter()
	scheduler.postScore = postscore.NewSelector()

	registry := plugin.NewRegistry()
	if err := plugin.RegisterBuiltins(registry); err != nil {
		return err
	}
	if err := plugin.ApplyGoExtensions(registry); err != nil {
		return err
	}
	if err := registry.RegisterFilterProvider(plugin.TypeExpression, expr.NewFilter); err != nil {
		return err
	}
	if err := registry.RegisterScoreProvider(plugin.TypeExpression, expr.NewScore); err != nil {
		return err
	}
	if err := registry.RegisterFilterProvider(plugin.TypeGRPC, grpcplugin.NewFilter); err != nil {
		return err
	}
	if err := registry.RegisterScoreProvider(plugin.TypeGRPC, grpcplugin.NewScore); err != nil {
		return err
	}
	profiles, err := profile.Compile(ctx, config.GetConfig(), registry)
	if err != nil {
		return err
	}
	scheduler.registry = registry
	scheduler.profiles.Store(profiles)
	config.AppendConfigWatcher(&schedulerConfigWatcher{ctx: ctx, registry: registry})
	score.StartAsyncScore(ctx)

	initTask(ctx)

	startClusterGaugeCollector(ctx)
	return nil
}

type schedulerConfigWatcher struct {
	ctx      context.Context
	registry *plugin.Registry
}

func (w *schedulerConfigWatcher) OnEvent(updated *config.Config) {
	compiled, err := profile.Compile(w.ctx, updated, w.registry)
	if err != nil {
		log.G(w.ctx).Errorf("scheduler profile reload rejected; keeping previous pipeline: %v", err)
		return
	}
	previous := scheduler.profiles.Swap(compiled)
	if previous != nil {
		// Close is lease-aware: in-flight scheduling calls keep using the old
		// immutable set and its external plugin connections until they finish.
		_ = previous.Close()
	}
	log.G(w.ctx).Infof("scheduler profiles reloaded: %v", compiled.Names())
}
