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
	registry        *plugin.Registry
	profiles        atomic.Pointer[profile.Set]
}{filter: make([]filter.Selector, 0)}

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
	// The async refresher writes n.Score on every cached node each
	// ScoreInterval; keep it inert while no compiled pipeline (the legacy
	// fallback included) binds the scorer. Evaluated per tick so a hot reload
	// that newly binds or drops the scorer takes effect without a restart.
	score.StartAsyncScore(ctx, func() bool {
		profiles := scheduler.profiles.Load()
		return profiles != nil && profiles.UsesScore(score.MultiFactorWeightedAverage)
	})

	initTask(ctx)
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
		if err := previous.Close(); err != nil {
			log.G(w.ctx).Errorf("scheduler profiles: closing the retired set: %v", err)
		}
	}
	log.G(w.ctx).Infof("scheduler profiles reloaded: %v", compiled.Names())
}
