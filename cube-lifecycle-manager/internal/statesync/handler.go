// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package statesync reconciles the CLM's view of a sandbox's runtime
// state with pause / resume actions driven externally through CubeMaster.
//
// When the SDK calls Sandbox.connect(), CubeMaster restores through Cubelet
// and writes a short-lived running marker under its lifecycle lock. The state
// event then updates CLM's registry, activity timestamp and CubeProxy state.
//
// CubeMaster now emits an OpState event on the lifecycle events stream after
// every successful pause / resume RPC (see CubeMaster/pkg/lifecycle/store.go
// PublishState). This package consumes those events and reconciles:
//
//   - Redis state key (cube:v1:shared:sandbox:lifecycle:state:<id>)
//   - CubeProxy's cube_sandbox_state dict (via proxypush.SetState)
//   - registry.Entry.LastActiveMs when the new state is "running"
//     (mirrors resumer.doResume: avoids the sweeper re-pausing a sandbox
//     that just came back).
//
// Handle skips events while a pausing/resuming marker is present to avoid
// overwriting an in-flight CLM transition. A completed Master resume replaces
// that marker with running before publishing its event, allowing reconciliation
// here. CLM's sweeper uses CAS and retained repairs for superseded pauses; its
// resumer also completes bookkeeping on a resume_completed partial failure.
package statesync

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/leader"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/lifecycle"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/redisstream"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/registry"
)

// Deps bundles the dependencies a Handle call needs. Constructed once at
// startup in main.go and reused for every event.
type Deps struct {
	Registry  *registry.Registry
	Redis     stateStore
	ProxyPush stateNotifier
	// TTL is the state key TTL applied when we SET the new state. Uses the
	// same value as sweeper/resumer's StateLockTTL so a state written from
	// this path is indistinguishable from one written by the CLM itself.
	TTL time.Duration
	Log *zap.Logger
	// Now returns the current time. Injectable for tests.
	Now func() time.Time

	// Leader gates Redis state-key writes and CubeProxy upserts. Standbys
	// only retain the in-memory registry; promotion reconcile writes the
	// shared key afterwards. Nil means "always write" (single-replica / tests).
	Leader leader.Status
}

func writeEnabled(d Deps) bool {
	return d.Leader == nil || d.Leader.IsLeader()
}

func recordWarmState(d Deps, sandboxID, newState string, now func() time.Time) {
	if newState == lifecycle.StateRunning {
		d.Registry.MergeLastActive(sandboxID, now().UnixMilli())
	}
	d.Registry.SetRuntimeState(sandboxID, newState)
}

// Handle applies a single OpState event. It is intentionally best-effort:
// partial failures are logged and swallowed so a Redis blip cannot poison
// the stream consumer loop.
func Handle(ctx context.Context, d Deps, ev redisstream.Event) {
	log := d.Log
	if log == nil {
		log = zap.NewNop()
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}

	if ev.SandboxID == "" {
		log.Warn("state event missing sandbox id")
		return
	}
	if ev.State == nil {
		log.Warn("state event missing payload",
			zap.String("sandbox_id", ev.SandboxID),
			zap.String("stream_id", ev.StreamID))
		return
	}

	newState := ev.State.State
	if newState != lifecycle.StatePaused && newState != lifecycle.StateRunning {
		log.Warn("state event has invalid state",
			zap.String("sandbox_id", ev.SandboxID),
			zap.String("state", newState))
		return
	}

	if d.Registry == nil || d.Registry.Get(ev.SandboxID) == nil {
		// Sandbox is not (yet) known to the CLM. This can happen when
		// the state event overtakes its own create event (unlikely — same
		// stream, same partition — but cheap to guard). Ignore: the create
		// event will populate the registry and any subsequent state
		// reconciliation will pick things up.
		log.Warn("state event for unknown sandbox",
			zap.String("sandbox_id", ev.SandboxID),
			zap.String("state", newState))
		return
	}

	if !writeEnabled(d) {
		// Standbys consume one ordered XREAD sequence and retain the latest
		// terminal state for promotion, but never perform external writes.
		recordWarmState(d, ev.SandboxID, newState, now)
		return
	}

	cur, _, err := d.Redis.GetState(ctx, ev.SandboxID)
	if err != nil {
		log.Warn("state event: get current state failed",
			zap.String("sandbox_id", ev.SandboxID), zap.Error(err))
		return
	}

	// Transition-lock protection: the CLM itself is mid-flight and will
	// write the terminal state as part of tryPause / doResume. Applying an
	// external event here would race against that write and could clobber
	// a "resuming" lock with a "paused" verdict — the opposite of what we
	// want.
	if cur == "pausing" || cur == "resuming" {
		log.Info("state event skipped: transition in progress",
			zap.String("sandbox_id", ev.SandboxID),
			zap.String("cur", cur),
			zap.String("new", newState))
		return
	}

	updated, err := d.Redis.WriteStateCAS(ctx, ev.SandboxID, cur, newState, d.TTL)
	if err != nil {
		log.Warn("state event: set state failed",
			zap.String("sandbox_id", ev.SandboxID),
			zap.String("new", newState), zap.Error(err))
		return
	}
	if !updated {
		log.Info("state event skipped: state changed concurrently",
			zap.String("sandbox_id", ev.SandboxID),
			zap.String("cur", cur),
			zap.String("new", newState))
		return
	}
	recordWarmState(d, ev.SandboxID, newState, now)
	if d.ProxyPush != nil {
		if err := d.ProxyPush.SetState(ctx, ev.SandboxID, newState); err != nil {
			log.Warn("state event: push proxy state failed",
				zap.String("sandbox_id", ev.SandboxID),
				zap.String("new", newState), zap.Error(err))
		}
	}
	log.Info("state event applied",
		zap.String("sandbox_id", ev.SandboxID),
		zap.String("cur", cur),
		zap.String("new", newState),
		zap.String("actor", ev.State.Actor),
		zap.String("source", ev.State.Source))
}
