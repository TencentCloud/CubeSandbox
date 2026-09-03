// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package scheduler provides a scheduler for the cube-master
package scheduler

import (
	"context"
	"errors"
	"reflect"
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
	config.AppendConfigWatcher(&schedulerConfigWatcher{ctx: ctx, registry: registry, lastCompiled: config.GetConfig().Scheduler})
	// The async refresher writes n.Score on every cached node each
	// ScoreInterval; keep it inert while no compiled pipeline (the legacy
	// fallback included) binds the scorer AND the legacy enable_scorers
	// condition is unmet. node.Score is also consumed directly by the
	// operator endpoints (info ScoreOnly, stat "score"/"pscore"), so the
	// legacy term keeps those endpoints fed for deployments that configure
	// scorers without any profile binding them — replicating the old
	// NewSelector gate (resource_weights set and enable_scorers non-empty).
	// Evaluated per tick so a hot reload that newly binds or drops the
	// scorer takes effect without a restart.
	score.StartAsyncScore(ctx, func() bool {
		if profiles := scheduler.profiles.Load(); profiles != nil &&
			profiles.UsesScore(score.MultiFactorWeightedAverage) {
			return true
		}
		sc := config.GetConfig().Scheduler
		return sc != nil && sc.Score != nil && sc.Score.ResourceWeights != nil &&
			len(sc.Score.EnableScorers) > 0
	})

	initTask(ctx)
	return nil
}

type schedulerConfigWatcher struct {
	ctx      context.Context
	registry *plugin.Registry
	// lastCompiled is the scheduler section the live profile set was
	// compiled from. Hot reloads deliver a freshly decoded Config graph (the
	// global pointer is swapped, never mutated in place), so a DeepEqual
	// against the previous section is a sound change detector.
	lastCompiled *config.WrapperSchedulerConf
}

func (w *schedulerConfigWatcher) OnEvent(updated *config.Config) {
	// Recompile only when the scheduler section actually changed: Compile
	// reads nothing outside it, and a full recompile re-dials and
	// re-handshakes every external plugin socket — on an unrelated reload
	// that both churns connections and lets a momentarily-unreachable
	// plugin reject a reload whose global config has already swapped in.
	if updated == nil || reflect.DeepEqual(updated.Scheduler, w.lastCompiled) {
		return
	}
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
	// Only a successfully compiled-and-swapped section becomes the
	// fingerprint: a rejected reload must be retried by the next event, not
	// remembered as if it had taken effect.
	w.lastCompiled = updated.Scheduler
	log.G(w.ctx).Infof("scheduler profiles reloaded: %v", compiled.Names())
}
