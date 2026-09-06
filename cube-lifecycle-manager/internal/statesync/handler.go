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

	// Leader gates CubeProxy writes. Standbys never push the fleet.
	Leader leader.Status
	// Persister gates shared Redis state-key writes. During promotion
	// catch-up the replica already holds the lease but is not yet an
	// executable leader; it must still CAS the key so hydrate does not
	// replay a stale value. Nil means "same as Leader".
	Persister leader.Status
}

func persistEnabled(d Deps) bool {
	p := d.Persister
	if p == nil {
		p = d.Leader
	}
	return p == nil || p.IsLeader()
}

func pushEnabled(d Deps) bool {
	return d.Leader == nil || d.Leader.IsLeader()
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

	recordWarmState := func() {
		if newState == lifecycle.StateRunning {
			d.Registry.MergeLastActive(ev.SandboxID, now().UnixMilli())
		}
		d.Registry.SetRuntimeState(ev.SandboxID, newState)
	}
	if !persistEnabled(d) {
		// Standbys consume one ordered XREAD sequence and retain the latest
		// terminal state for promotion, but never perform external writes.
		recordWarmState()
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

	updated, err := d.Redis.WriteStateCAS(ctx, ev.SandboxID, cur, newState, ev.StreamID, d.TTL)
	if err != nil {
		log.Warn("state event: set state failed",
			zap.String("sandbox_id", ev.SandboxID),
			zap.String("new", newState), zap.Error(err))
		return
	} else if !updated {
		log.Info("state event skipped: state changed concurrently",
			zap.String("sandbox_id", ev.SandboxID),
			zap.String("cur", cur),
			zap.String("new", newState))
		return
	}
	recordWarmState()
	if pushEnabled(d) && d.ProxyPush != nil {
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
