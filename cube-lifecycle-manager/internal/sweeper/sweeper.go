// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package sweeper periodically scans the registry for sandboxes that have
// exceeded their idle timeout and triggers a pause via CubeMaster. It is the
// auto-pause half of the system; resume runs on demand from the HTTP server.
package sweeper

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/cubemasterclient"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/leader"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/lifecycle"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/registry"
)

// Options is the dependency injection bundle for Sweeper. Pulling everything
// through here keeps the sweep logic itself a pure function of inputs and
// lets tests substitute fakes for the Redis / RPC dependencies.
type Options struct {
	Registry           *registry.Registry
	Redis              stateStore
	CubeMaster         pauseKiller
	ProxyPush          stateNotifier
	DefaultIdleTimeout time.Duration

	// BootstrapWarmup delays the first sweep after CLM starts so the
	// last_active poller has a chance to populate activity timestamps for
	// sandboxes that were already running before this CLM instance came
	// up. Without this delay, a fresh CLM would observe LastActiveMs=0
	// for every bootstrap entry and immediately try to pause anything past
	// its idle deadline — even if the sandbox has been actively serving
	// traffic on the proxy.
	//
	// Set to 0 in tests that drive sweepOnce manually.
	BootstrapWarmup time.Duration

	StateLockTTL time.Duration
	Interval     time.Duration

	// StartedAt is CLM's process start time. Used as the boundary
	// between "bootstrap" and "stream" entries for the warmup gate. When
	// zero, defaults to Now() at construction time.
	StartedAt time.Time

	// ActionTimeout bounds individual pause or kill decision execution.
	ActionTimeout time.Duration

	Now func() time.Time // injectable for tests
	Log *zap.Logger

	// Leader gates singleton pause/kill work. Nil preserves the historical
	// single-instance behavior for tests and deployments with election off.
	Leader leader.Status
}

// Sweeper iterates the registry on a fixed interval. It is intended to run as
// its own goroutine; Run returns when ctx is cancelled.
type Sweeper struct {
	o Options
	// Owned by the sweep goroutine. Failed repairs survive individual action
	// deadlines and are retried before the paused fast path on later ticks.
	pending map[string]struct{}
	// Notification retries never gate lifecycle decisions or extend activity.
	notifications map[string]struct{}
	// metrics — exposed for testing rather than via Prometheus for now.
	pauseTriggered atomic.Int64
	pauseFailed    atomic.Int64
	killTriggered  atomic.Int64
	killFailed     atomic.Int64
}

func New(o Options) *Sweeper {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.StartedAt.IsZero() {
		o.StartedAt = o.Now()
	}
	return &Sweeper{o: o, pending: make(map[string]struct{}), notifications: make(map[string]struct{})}
}

// Run blocks until ctx is cancelled, sweeping every Interval.
func (s *Sweeper) Run(ctx context.Context) error {
	t := time.NewTicker(s.o.Interval)
	defer t.Stop()
	// Don't fire on the first tick; let the registry warm up via Bootstrap +
	// the initial last_active poll. The next tick is one Interval away.
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			s.sweepOnce(ctx)
		}
	}
}

// sweepOnce is exported (lowercase but called via tests in the same package)
// so the test can drive a single iteration deterministically.
func (s *Sweeper) sweepOnce(ctx context.Context) {
	if s.o.Leader != nil && !s.o.Leader.IsLeader() {
		return
	}
	// Bound the whole retry phase, not just each RPC, so a slow fleet cannot
	// delay idle decisions in proportion to the number of pending sandboxes.
	// Separate budgets prevent either queue from starving the other, while
	// preserving the time previously allowed for an individual attempt.
	retryCtx, retryCancel := context.WithTimeout(ctx, advisoryPushTimeout)
	remaining := retryBatchLimit
	for sid := range s.notifications {
		if retryCtx.Err() != nil || remaining == 0 {
			break
		}
		if _, repairing := s.pending[sid]; repairing {
			continue
		}
		if ctx.Err() != nil || (s.o.Leader != nil && !s.o.Leader.IsLeader()) {
			retryCancel()
			return
		}
		remaining--
		if err := s.retryNotification(retryCtx, sid); err != nil {
			s.o.Log.Warn("state notification pending", zap.String("sandbox_id", sid), zap.Error(err))
		}
	}
	retryCancel()
	retryCtx, retryCancel = context.WithTimeout(ctx, reconciliationTimeout)
	remaining = retryBatchLimit
	for sid := range s.pending {
		if retryCtx.Err() != nil || remaining == 0 {
			break
		}
		if ctx.Err() != nil || (s.o.Leader != nil && !s.o.Leader.IsLeader()) {
			retryCancel()
			return
		}
		remaining--
		if err := s.reconcilePause(retryCtx, sid); err != nil {
			s.o.Log.Warn("pause reconciliation pending", zap.String("sandbox_id", sid), zap.Error(err))
		}
	}
	retryCancel()

	now := s.o.Now()
	nowMs := now.UnixMilli()

	// Bootstrap-warmup gate: when CLM just started, hold off on
	// pausing entries that came in via HGETALL bootstrap (FirstSeenAt ≈
	// startedAt). Two reasons:
	//   * LastActiveMs hasn't been backfilled yet — first last_active
	//     poll lands ~LastActivePoll seconds in.
	//   * If the proxy is healthy, those sandboxes very likely received
	//     traffic in the last few seconds; we just don't know it yet.
	// New entries that arrive AFTER startup (FirstSeenAt > startedAt) do
	// not need this protection: their CreatedAt is recent and they are
	// immediately tracked by log_phase, so we can act on the standard
	// `idle = now - max(LastActiveMs, CreatedAt)` rule.
	withinWarmup := now.Sub(s.o.StartedAt) < s.o.BootstrapWarmup

	for _, e := range s.o.Registry.Snapshot() {
		if _, pending := s.pending[e.Meta.SandboxID]; pending {
			continue
		}
		// Bootstrap entries during warmup → skip. `FirstSeenAt <= StartedAt`
		// (we backdate FirstSeenAt during bootstrap, so equality means
		// "loaded from HGETALL", inequality means "new event").
		if withinWarmup && !e.FirstSeenAt.After(s.o.StartedAt) {
			continue
		}

		// Baseline = the most recent of (LastActiveMs, CreatedAt). For a
		// sandbox CLM has just observed via stream, LastActiveMs
		// is 0 until the next request arrives — fall through to CreatedAt
		// which always carries a real timestamp from CubeMaster.
		baseline := e.LastActiveMs
		if e.Meta.CreatedAt > baseline {
			baseline = e.Meta.CreatedAt
		}
		if baseline == 0 {
			// No CreatedAt either (legacy entry?) — fall back to FirstSeenAt
			// so we don't blindly pause on zero.
			baseline = e.FirstSeenAt.UnixMilli()
		}

		// Idle-timeout decision. See docs/guide/lifecycle.md.
		var timeout time.Duration
		if e.Meta.TimeoutSeconds == nil {
			timeout = s.o.DefaultIdleTimeout
		} else if ts := *e.Meta.TimeoutSeconds; ts < 0 {
			continue
		} else {
			timeout = time.Duration(ts) * time.Second
		}

		idleFor := time.Duration(nowMs-baseline) * time.Millisecond
		if idleFor < timeout {
			continue
		}

		// Already-terminal fast path: Redis or the in-memory RuntimeState
		// may already say the sandbox is parked at paused/pausing/killing/
		// killed. Redis alone is not enough — the state-key TTL
		// (StateLockTTL=60s) expires, GetState goes empty, and idle keeps
		// growing because LastActive is frozen after pause. Without the
		// RuntimeState check we re-issue Pause every Interval.
		if isParkedSweepState(e.RuntimeState) {
			continue
		}
		curState, _, stateErr := s.o.Redis.GetState(ctx, e.Meta.SandboxID)
		if stateErr != nil {
			s.o.Log.Warn("get state failed; will attempt action anyway",
				zap.String("sandbox_id", e.Meta.SandboxID),
				zap.Error(stateErr))
		} else if isParkedSweepState(curState) {
			// Nothing to do. "pausing" / "killing" mean a peer (or our own
			// previous invocation) is mid-flight; let it finish.
			continue
		}

		switch {
		case e.Meta.AutoPause:
			if s.o.Leader != nil && !s.o.Leader.IsLeader() {
				return
			}
			s.o.Log.Info("idle threshold exceeded; pausing",
				zap.String("sandbox_id", e.Meta.SandboxID),
				zap.Duration("idle_for", idleFor),
				zap.Intp("timeout_seconds", e.Meta.TimeoutSeconds))

			if err := s.tryPause(ctx, e); err != nil {
				s.pauseFailed.Add(1)
				s.o.Log.Warn("auto-pause failed",
					zap.String("sandbox_id", e.Meta.SandboxID),
					zap.Duration("idle_for", idleFor),
					zap.Error(err))
			}
		default:
			if s.o.Leader != nil && !s.o.Leader.IsLeader() {
				return
			}
			s.o.Log.Info("idle threshold exceeded; killing",
				zap.String("sandbox_id", e.Meta.SandboxID),
				zap.Duration("idle_for", idleFor),
				zap.Intp("timeout_seconds", e.Meta.TimeoutSeconds),
				zap.String("kill_reason", cubemasterclient.KillReasonTimeout))

			if err := s.tryKill(ctx, e); err != nil {
				s.killFailed.Add(1)
				s.o.Log.Warn("timeout-kill failed",
					zap.String("sandbox_id", e.Meta.SandboxID),
					zap.Duration("idle_for", idleFor),
					zap.Error(err))
			}
		}
	}
}

const (
	advisoryPushTimeout   = time.Second
	reconciliationTimeout = 3 * time.Second
	retryBatchLimit       = 16
	terminalWriteReserve  = 2 * time.Second
)

// rpcContext carves the CubeMaster call out of the action budget, holding
// terminalWriteReserve back so a slow pause/kill cannot starve the terminal
// state writes that follow it. parent always carries a deadline: it comes
// from context.WithTimeout(ctx, ActionTimeout).
func rpcContext(parent context.Context) (context.Context, context.CancelFunc) {
	deadline, _ := parent.Deadline()
	timeout := time.Until(deadline.Add(-terminalWriteReserve))
	if timeout <= 0 {
		timeout = time.Millisecond
	}
	return context.WithTimeout(parent, timeout)
}

// tryPause acquires the state lock, calls CubeMaster, and pushes the new
// state out to CubeProxy. It is idempotent — a lost SETNX race is treated as
// success (someone else is pausing the same sandbox).
//
// Two CubeMaster ret_codes are not real failures and are mapped to
// terminal-success behaviour:
//
//   - InvalidParamFormat / "key not found" → the sandbox is gone (deleted
//     out from under us, e.g. CubeMaster's own cleanup raced). We evict it
//     from the registry so the next sweep doesn't keep retrying forever
//     and don't leave the proxy seeing a stale "pausing" state.
//   - TaskStateInvalid / "sandbox is already paused" → the sandbox is
//     already where we wanted it (peer CLM replica / earlier failed-but-applied
//     attempt). Treat exactly like a fresh successful pause.
func (s *Sweeper) tryPause(ctx context.Context, e registry.Entry) error {
	ctx, cancel := context.WithTimeout(ctx, s.o.ActionTimeout)
	defer cancel()

	sid := e.Meta.SandboxID
	got, err := s.o.Redis.AcquireState(ctx, sid, "pausing", s.o.StateLockTTL)
	if err != nil {
		return err
	}
	if !got {
		// Another CLM replica (or our own resume handler) holds the state. Skip.
		return nil
	}

	// Tell CubeProxy first that the sandbox is pausing, so any new requests
	// hit the 503 retry path immediately and don't race the rpc.
	// Use a small slice of the budget so an unreachable proxy cannot starve the RPC.
	prePushCtx, preCancel := context.WithTimeout(ctx, advisoryPushTimeout)
	if err := s.o.ProxyPush.SetState(prePushCtx, sid, "pausing"); err != nil {
		s.o.Log.Warn("push pausing state failed",
			zap.String("sandbox_id", sid), zap.Error(err))
		// Continue anyway — the rpc and final state push are still useful.
	}
	preCancel()

	rpcCtx, rpcCancel := rpcContext(ctx)
	pauseErr := s.o.CubeMaster.Pause(rpcCtx, sid, e.Meta.InstanceType)
	rpcCancel()
	if pauseErr != nil {
		var apiErr *cubemasterclient.APIError
		switch {
		case errors.As(pauseErr, &apiErr) && apiErr.IsPauseSuperseded():
			s.pending[sid] = struct{}{}
			return s.reconcileSupersededPause(ctx, sid)
		case errors.As(pauseErr, &apiErr) && apiErr.IsNotFound():
			// Sandbox doesn't exist on CubeMaster anymore. Clean up local
			// state and stop chasing it.
			_ = s.o.Redis.ClearStateNotify(ctx, sid)
			_ = s.o.ProxyPush.DeleteMeta(ctx, sid)
			s.o.Registry.Delete(sid)
			s.o.Log.Info("sandbox not found on cubemaster; evicting from registry",
				zap.String("sandbox_id", sid),
				zap.Int("ret_code", apiErr.RetCode),
				zap.String("ret_msg", apiErr.RetMsg))
			return nil
		case errors.As(pauseErr, &apiErr) && apiErr.IsAlreadyInState():
			// CubeMaster says the sandbox is already paused. Fall through
			// to the success path so we still write `paused` state to
			// Redis + push it to CubeProxy (in case a peer CLM replica paused
			// it but didn't push, or our previous attempt failed only on
			// the post-RPC bookkeeping).
			s.o.Log.Info("sandbox already paused on cubemaster; reconciling state",
				zap.String("sandbox_id", sid),
				zap.Int("ret_code", apiErr.RetCode))
			// no return — proceed to success bookkeeping below
		default:
			// A transport or timeout error has an unknown server-side result.
			// Do NOT clear state and broadcast "running": if the VM was actually
			// paused, CubeProxy will route requests to a frozen VM without resume.
			// Retain "pausing" ownership for the ambiguity window so a concurrent or
			// subsequent check does not race.
			if !errors.As(pauseErr, &apiErr) {
				return errors.New("cubemaster pause result unknown: " + pauseErr.Error())
			}
			// Real failure (definitive APIError). Roll back: clear the pausing
			// state so a future sweep can retry, and tell CubeProxy the sandbox
			// is back to running (it never actually paused).
			_ = s.o.Redis.ClearStateNotify(ctx, sid)
			_ = s.o.ProxyPush.SetState(ctx, sid, "running")
			return errors.New("cubemaster pause: " + pauseErr.Error())
		}
	}

	s.pending[sid] = struct{}{}
	s.o.Registry.SetRuntimeState(sid, lifecycle.StatePaused)
	var pushErr error
	if err := s.o.ProxyPush.SetState(ctx, sid, "paused"); err != nil {
		pushErr = err
		s.o.Log.Warn("push paused state failed",
			zap.String("sandbox_id", sid), zap.Error(err))
	}
	// A resume may complete after the pause RPC but before this bookkeeping.
	// Finish proxy writes before the CAS so a failed CAS can repair them, and
	// never overwrite the newer running marker with this stale paused result.
	updated, err := s.o.Redis.WriteStateCAS(ctx, sid, "pausing", "paused", s.o.StateLockTTL)
	if err != nil {
		return err
	}
	if !updated {
		return s.reconcileSupersededPause(ctx, sid)
	}
	delete(s.pending, sid)
	if pushErr != nil {
		s.notifications[sid] = struct{}{}
	} else {
		delete(s.notifications, sid)
	}

	s.pauseTriggered.Add(1)
	s.o.Log.Info("auto-paused sandbox",
		zap.String("sandbox_id", sid),
		zap.Intp("timeout_seconds", e.Meta.TimeoutSeconds))
	return nil
}

func (s *Sweeper) reconcileSupersededPause(ctx context.Context, sid string) error {
	// The pause's action context may already have expired. Bound each repair
	// independently; retain unresolved state repairs across Redis failures.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reconciliationTimeout)
	defer cancel()
	return s.reconcilePause(ctx, sid)
}

// Scheduled retries inherit the sweep's shared budget; immediate post-RPC
// repairs use the independent context above when the action has expired.
func (s *Sweeper) reconcilePause(ctx context.Context, sid string) error {
	entry := s.o.Registry.Get(sid)
	if entry == nil {
		delete(s.pending, sid)
		return nil
	}
	state, _, err := s.o.Redis.GetState(ctx, sid)
	if err != nil {
		return err
	}
	if state == "" {
		state, err = s.o.CubeMaster.SandboxState(ctx, sid, entry.Meta.InstanceType)
		if err != nil {
			return err
		}
		updated, err := s.o.Redis.WriteStateCAS(ctx, sid, "", state, s.o.StateLockTTL)
		if err != nil {
			return err
		}
		if !updated {
			return errors.New("lifecycle state changed during reconciliation")
		}
	}
	if state != lifecycle.StateRunning && state != lifecycle.StatePaused {
		return errors.New("lifecycle transition still in progress")
	}
	if state == lifecycle.StateRunning && entry.RuntimeState != lifecycle.StateRunning {
		s.o.Registry.MergeLastActive(sid, s.o.Now().UnixMilli())
	}
	s.o.Registry.SetRuntimeState(sid, state)
	current, _, err := s.o.Redis.GetState(ctx, sid)
	if err != nil {
		return err
	}
	if current != state {
		return errors.New("lifecycle state changed during reconciliation")
	}
	delete(s.pending, sid)
	s.notifications[sid] = struct{}{}
	return s.retryNotification(ctx, sid)
}

// Resolve the latest state on every attempt; a queued notification must not
// replay running after a subsequent pause or extend the Redis ownership TTL.
func (s *Sweeper) retryNotification(ctx context.Context, sid string) error {
	ctx, cancel := context.WithTimeout(ctx, advisoryPushTimeout)
	defer cancel()
	currentState := func() (string, error) {
		entry := s.o.Registry.Get(sid)
		if entry == nil {
			return "", nil
		}
		state, _, err := s.o.Redis.GetState(ctx, sid)
		if err == nil && state == "" {
			state = entry.RuntimeState
		}
		return state, err
	}
	state, err := currentState()
	if err != nil {
		return err
	}
	if state == "" {
		if s.o.Registry.Get(sid) != nil {
			return errors.New("sandbox state not yet known")
		}
		delete(s.notifications, sid)
		return nil
	}
	if err := s.o.ProxyPush.SetState(ctx, sid, state); err != nil {
		return err
	}
	current, err := currentState()
	if err != nil {
		return err
	}
	if current != state {
		return errors.New("lifecycle state changed during proxy notification")
	}
	delete(s.notifications, sid)
	return nil
}

// Stats returns the running counters, useful for tests and /metrics.
func (s *Sweeper) Stats() (triggered, failed int64) {
	return s.pauseTriggered.Load(), s.pauseFailed.Load()
}

// KillStats returns the kill-path counters, kept separate from Stats() so
// existing callers / tests that only care about the pause path don't have to
// change. Mirrors the (triggered, failed) shape of Stats.
func (s *Sweeper) KillStats() (triggered, failed int64) {
	return s.killTriggered.Load(), s.killFailed.Load()
}

// tryKill is the kill-path counterpart of tryPause. Same coordination
// pattern (SETNX → notify proxy → RPC → finalise), but the terminal state is
// non-recoverable: on success we evict the registry entry and tell every
// CubeProxy replica to forget the sandbox. The Lua gate maps `killing` /
// `killed` to 410 Gone so any in-flight client request fails fast instead of
// hanging on a doomed retry.
func (s *Sweeper) tryKill(ctx context.Context, e registry.Entry) error {
	ctx, cancel := context.WithTimeout(ctx, s.o.ActionTimeout)
	defer cancel()

	sid := e.Meta.SandboxID
	got, err := s.o.Redis.AcquireState(ctx, sid, "killing", s.o.StateLockTTL)
	if err != nil {
		return err
	}
	if !got {
		// A peer CLM replica (or our own resume / pause path) holds the state.
		// Skip — the holder will drive the transition.
		return nil
	}

	prePushCtx, preCancel := context.WithTimeout(ctx, advisoryPushTimeout)
	if err := s.o.ProxyPush.SetState(prePushCtx, sid, "killing"); err != nil {
		s.o.Log.Warn("push killing state failed",
			zap.String("sandbox_id", sid), zap.Error(err))
	}
	preCancel()

	rpcCtx, rpcCancel := rpcContext(ctx)
	killErr := s.o.CubeMaster.Kill(rpcCtx, sid, e.Meta.InstanceType, cubemasterclient.KillReasonTimeout)
	rpcCancel()
	if killErr != nil {
		var apiErr *cubemasterclient.APIError
		switch {
		case errors.As(killErr, &apiErr) && apiErr.IsNotFound():
			s.o.Log.Info("sandbox not found on cubemaster during kill; evicting from registry",
				zap.String("sandbox_id", sid),
				zap.Int("ret_code", apiErr.RetCode),
				zap.String("ret_msg", apiErr.RetMsg))
		case errors.As(killErr, &apiErr) && apiErr.IsAlreadyInState():
			s.o.Log.Info("sandbox already in terminal state on cubemaster; reconciling",
				zap.String("sandbox_id", sid),
				zap.Int("ret_code", apiErr.RetCode))
		default:
			// A transport or timeout error has an unknown server-side result.
			// Do NOT clear state and broadcast "running". Retain "killing"
			// marker so proxy continues to reject requests and sweeper does not flap.
			if !errors.As(killErr, &apiErr) {
				return errors.New("cubemaster kill result unknown: " + killErr.Error())
			}
			_ = s.o.Redis.ClearStateNotify(ctx, sid)
			_ = s.o.ProxyPush.SetState(ctx, sid, "running")
			return errors.New("cubemaster kill: " + killErr.Error())
		}
	}

	if err := s.o.Redis.WriteState(ctx, sid, "killed", s.o.StateLockTTL); err != nil {
		s.o.Log.Warn("write killed state failed",
			zap.String("sandbox_id", sid), zap.Error(err))
	}
	if err := s.o.ProxyPush.DeleteMeta(ctx, sid); err != nil {
		s.o.Log.Warn("delete meta after kill failed",
			zap.String("sandbox_id", sid), zap.Error(err))
	}
	s.o.Registry.Delete(sid)

	s.killTriggered.Add(1)
	s.o.Log.Info("timeout-killed sandbox",
		zap.String("sandbox_id", sid),
		zap.Intp("timeout_seconds", e.Meta.TimeoutSeconds),
		zap.String("kill_reason", cubemasterclient.KillReasonTimeout))
	return nil
}

func isParkedSweepState(state string) bool {
	switch state {
	case lifecycle.StatePaused, "pausing", "killing", lifecycle.StateKilled:
		return true
	default:
		return false
	}
}
