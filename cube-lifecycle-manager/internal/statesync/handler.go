// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package statesync reconciles the CLM's view of a sandbox's runtime
// state with pause / resume actions driven externally through CubeMaster.
//
// CubeMaster is a stateless proxy: when the SDK calls Sandbox.connect() to
// resume a paused sandbox, CubeMaster forwards the RPC to Cubelet but does
// not touch the CLM's state key or CubeProxy's per-worker state dict.
// Without a nudge from CubeMaster the CLM would keep thinking the
// sandbox is paused and reject the next request from the dataplane.
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
// The event source (CubeMaster) is stateless with respect to the CLM's
// SETNX-based transition locks ("pausing", "resuming"). To avoid a state
// event racing an in-flight sweeper.tryPause or resumer.doResume, Handle
// skips reconciliation whenever the current state key holds a transition
// marker; the CLM's own flow will write the terminal state moments
// later, so the event is redundant.
package statesync

import (
	"context"
	"time"

	"go.uber.org/zap"

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

	// Idempotence: already at the desired state.
	if cur == newState {
		return
	}

	if err := d.Redis.SetState(ctx, ev.SandboxID, newState, d.TTL); err != nil {
		log.Warn("state event: set state failed",
			zap.String("sandbox_id", ev.SandboxID),
			zap.String("new", newState), zap.Error(err))
		// Continue: try to push to proxy anyway so at least one side
		// converges. Next sweep will retry Redis via its own path.
	}
	if d.ProxyPush != nil {
		if err := d.ProxyPush.SetState(ctx, ev.SandboxID, newState); err != nil {
			log.Warn("state event: push proxy state failed",
				zap.String("sandbox_id", ev.SandboxID),
				zap.String("new", newState), zap.Error(err))
		}
	}
	if newState == lifecycle.StateRunning {
		// Mirror resumer.doResume: bump LastActiveMs so sweeper won't
		// immediately re-pause a sandbox that was just resumed by the
		// user. The proxy's log_phase will eventually overwrite this via
		// last_active polling, but we want an accurate baseline now.
		d.Registry.MergeLastActive(ev.SandboxID, now().UnixMilli())
	}
	log.Info("state event applied",
		zap.String("sandbox_id", ev.SandboxID),
		zap.String("cur", cur),
		zap.String("new", newState),
		zap.String("actor", ev.State.Actor),
		zap.String("source", ev.State.Source))
}
