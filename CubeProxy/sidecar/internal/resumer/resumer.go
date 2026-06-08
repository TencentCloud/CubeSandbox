// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package resumer is the request-side counterpart to sweeper. CubeProxy
// posts to /internal/resume when it sees a paused sandbox; this package
// serializes the work so concurrent dataplane requests for the same sandbox
// share a single resume RPC.
package resumer

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/tencentcloud/CubeSandbox/CubeProxy/sidecar/internal/cubemasterclient"
	"github.com/tencentcloud/CubeSandbox/CubeProxy/sidecar/internal/registry"
)

// Options bundles dependencies for the Resumer. Concrete production wiring
// lives in cmd/sidecar/main.go; the interface types defined in iface.go let
// tests substitute fakes for Redis / CubeMaster / CubeProxy.
type Options struct {
	Registry     *registry.Registry
	Redis        stateStore
	CubeMaster   resumePauser
	ProxyPush    stateNotifier
	StateLockTTL time.Duration
	Log          *zap.Logger
}

// Resumer coalesces concurrent resume requests for the same sandbox into a
// single in-flight call to CubeMaster. It does NOT cache outcomes — every
// caller that arrives after a resume completes will see the registry/state
// already updated and return immediately, but a fresh paused→resume cycle
// must be allowed to fire a new RPC.
type Resumer struct {
	o     Options
	mu    sync.Mutex
	calls map[string]*call
}

// call represents one in-flight resume operation. Every goroutine waiting on
// the same sandbox blocks on done; the first arrival drives the work.
type call struct {
	done chan struct{}
	err  error
}

// New constructs a Resumer.
func New(o Options) *Resumer {
	return &Resumer{
		o:     o,
		calls: make(map[string]*call),
	}
}

// Resume drives the sandbox back to running. Safe to call concurrently with
// the same sandboxID; only one CubeMaster RPC fires per outstanding paused
// state.
func (r *Resumer) Resume(ctx context.Context, sandboxID string) error {
	if sandboxID == "" {
		return errors.New("empty sandbox_id")
	}

	r.mu.Lock()
	if c, ok := r.calls[sandboxID]; ok {
		r.mu.Unlock()
		select {
		case <-c.done:
			return c.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c := &call{done: make(chan struct{})}
	r.calls[sandboxID] = c
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		delete(r.calls, sandboxID)
		r.mu.Unlock()
		close(c.done)
	}()

	c.err = r.doResume(ctx, sandboxID)
	return c.err
}

func (r *Resumer) doResume(ctx context.Context, sandboxID string) error {
	entry := r.o.Registry.Get(sandboxID)
	if entry == nil {
		return errors.New("sandbox not in registry")
	}
	if !entry.Meta.AutoResume {
		return errors.New("auto_resume not enabled for sandbox")
	}

	// cube:sandbox:state:<id> is dual-purpose: terminal markers (paused /
	// running) AND in-flight transition locks (pausing / resuming) live in
	// the same key. SETNX alone can't distinguish "I'm resuming this" from
	// "this sandbox is parked at terminal-paused waiting for someone to
	// resume it" — we need to peek first.
	switch ownErr := r.acquireResumeOwnership(ctx, sandboxID); {
	case ownErr == nil:
		// We own the resume; call CubeMaster.
		if err := r.callCubeMasterResume(ctx, sandboxID, entry.Meta.InstanceType); err != nil {
			return err
		}
	case errors.Is(ownErr, errAlreadyRunning):
		// Sandbox is already running per Redis. Skip the RPC and run the
		// success bookkeeping below so we re-assert state to the proxy.
		// (This is the case where a peer resume already completed but the
		// proxy's local dict still says "paused".)
	default:
		return ownErr
	}

	// Success bookkeeping. Three writes, all best-effort:
	//
	//  1. Redis state → "running" so the next request from any sidecar
	//     instance sees the right state.
	//  2. CubeProxy local state dict → "running" so the rewrite_phase gate
	//     stops triggering resumes for this sandbox.
	//  3. In-memory registry LastActiveMs → now. Without (3) the sweeper
	//     sees a stale baseline (LastActiveMs=0, CreatedAt from minutes
	//     ago) and immediately on the next 5s tick logs "idle threshold
	//     exceeded; pausing" for the sandbox we *just* woke up. The log
	//     is mostly cosmetic — tryPause will SETNX-fail against
	//     state=running and silently return — but the noise is
	//     misleading. The proxy's log_phase will eventually overwrite
	//     this via the periodic last_active poll, but we want the right
	//     answer immediately, not 5–10 seconds later.
	if err := r.o.Redis.SetState(ctx, sandboxID, "running", r.o.StateLockTTL); err != nil {
		r.o.Log.Warn("write running state failed",
			zap.String("sandbox_id", sandboxID), zap.Error(err))
	}
	if err := r.o.ProxyPush.SetState(ctx, sandboxID, "running"); err != nil {
		// Best-effort; CubeProxy locally also flips state to running on a
		// successful resume sub-request, so this is a safety net.
		r.o.Log.Warn("push running state failed",
			zap.String("sandbox_id", sandboxID), zap.Error(err))
	}
	r.o.Registry.MergeLastActive(sandboxID, time.Now().UnixMilli())

	r.o.Log.Info("auto-resumed sandbox", zap.String("sandbox_id", sandboxID))
	return nil
}

// callCubeMasterResume issues the resume RPC and maps the three classes
// of CubeMaster response (success / not-found / already-running / real
// failure) onto the appropriate caller-side cleanup. Returns nil when the
// caller should proceed to the success-bookkeeping path; non-nil when the
// caller should bail with an error.
func (r *Resumer) callCubeMasterResume(ctx context.Context, sandboxID, instanceType string) error {
	resumeErr := r.o.CubeMaster.Resume(ctx, sandboxID, instanceType)
	if resumeErr == nil {
		return nil
	}
	var apiErr *cubemasterclient.APIError
	switch {
	case errors.As(resumeErr, &apiErr) && apiErr.IsNotFound():
		// CubeMaster doesn't know this sandbox anymore — deleted out
		// from under us. Evict everywhere and surface as an error to
		// the HTTP caller so CubeProxy returns 5xx (the dataplane
		// request can't be served either way).
		_ = r.o.Redis.ClearState(ctx, sandboxID)
		_ = r.o.ProxyPush.DeleteMeta(ctx, sandboxID)
		r.o.Registry.Delete(sandboxID)
		r.o.Log.Info("sandbox not found on cubemaster during resume; evicted",
			zap.String("sandbox_id", sandboxID),
			zap.Int("ret_code", apiErr.RetCode),
			zap.String("ret_msg", apiErr.RetMsg))
		return errors.New("sandbox no longer exists")
	case errors.As(resumeErr, &apiErr) && apiErr.IsAlreadyInState():
		// Already running. Reconcile state and return success — the
		// dataplane request can proceed.
		r.o.Log.Info("sandbox already running on cubemaster; reconciling state",
			zap.String("sandbox_id", sandboxID),
			zap.Int("ret_code", apiErr.RetCode))
		return nil
	default:
		// Real failure: clear the resuming key so a future request can
		// retry, and surface the error.
		_ = r.o.Redis.ClearState(ctx, sandboxID)
		return errors.New("cubemaster resume: " + resumeErr.Error())
	}
}

// acquireResumeOwnership decides whether the current call should drive
// the resume RPC, wait for a peer to finish, or return immediately. It
// returns nil when the caller owns the resume (i.e. has the lock written
// as "resuming"); a non-nil error when the caller should NOT proceed
// (peer in flight resolved, sandbox already running, or real failure).
//
// The state-key conflict (terminal markers vs. transition locks share
// the key) is resolved by GET-ing the current value:
//
//   - "paused" or expired:        we own the resume — write "resuming"
//   - "running":                   nothing to do, return nil-and-success
//                                  via a sentinel (the caller's success
//                                  bookkeeping then runs and re-asserts
//                                  state, which is the right behaviour
//                                  for race-recovery).
//   - "pausing" or "resuming":    a peer is in flight → waitForRunning
//
// This is intentionally racy: between GET and SET another sidecar could
// claim the key. That's fine because the worst case is two resumers both
// calling CubeMaster.Resume — which CubeMaster handles idempotently
// (returns "already running" the second time, which we already map to
// success in the caller).
func (r *Resumer) acquireResumeOwnership(ctx context.Context, sandboxID string) error {
	cur, ok, err := r.o.Redis.GetState(ctx, sandboxID)
	if err != nil {
		return err
	}

	switch {
	case !ok, cur == "paused":
		// Either no lock at all (most common after sweeper's TTL expired)
		// or terminal "paused" left by a successful sweep. Either way we
		// claim ownership by SET-ing "resuming".
		if err := r.o.Redis.SetState(ctx, sandboxID, "resuming", r.o.StateLockTTL); err != nil {
			return err
		}
		return nil
	case cur == "running":
		// Sandbox is already running on Redis's view. No-op resume; the
		// caller's success path will re-push running to the proxy in case
		// the local dict drifted.
		r.o.Log.Info("resume requested but sandbox already running; reconciling",
			zap.String("sandbox_id", sandboxID))
		return errAlreadyRunning
	case cur == "pausing" || cur == "resuming":
		// Active transition by a peer → wait it out. waitForRunning
		// returning nil means the peer transitioned to "running"; treat
		// that as a no-op resume from our perspective so we DON'T issue
		// our own duplicate RPC. Any other return value (peer-paused,
		// lock expired, ctx done) propagates as an error.
		if err := r.waitForRunning(ctx, sandboxID); err != nil {
			return err
		}
		return errAlreadyRunning
	default:
		// Unknown state — fall back to wait, same translation rule.
		r.o.Log.Warn("unknown state during resume ownership probe",
			zap.String("sandbox_id", sandboxID),
			zap.String("state", cur))
		if err := r.waitForRunning(ctx, sandboxID); err != nil {
			return err
		}
		return errAlreadyRunning
	}
}

// errAlreadyRunning is returned by acquireResumeOwnership when GetState
// reports the sandbox is already running. The doResume caller treats it
// as a successful no-op (state will be re-asserted into the proxy dict).
var errAlreadyRunning = errors.New("sandbox already running")

// waitForRunning is invoked when AcquireState lost the SETNX race — i.e.
// some other key/holder occupies cube:sandbox:state:<id>. We poll that key
// for one of three terminal outcomes:
//
//   - state == "running"    → peer succeeded, request can proceed.
//   - state == "paused"     → peer gave up; bail with an error so
//                              CubeProxy returns 503 and the next request
//                              gets a fresh resume attempt.
//   - key expired (!ok)     → peer crashed mid-flight; do NOT treat this
//                              as success — return a clear error so the
//                              caller can retry. Without this guard we
//                              would silently let through a request to a
//                              still-paused sandbox.
func (r *Resumer) waitForRunning(ctx context.Context, sandboxID string) error {
	const pollEvery = 200 * time.Millisecond
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	for {
		state, ok, err := r.o.Redis.GetState(ctx, sandboxID)
		if err != nil {
			return err
		}
		switch {
		case !ok:
			// Peer's lock expired without writing a terminal state. Don't
			// pretend everything is fine — the sandbox is in an unknown
			// state. The caller will surface a 503 and the next request
			// re-enters Resume() and re-acquires the lock cleanly.
			return errors.New("peer resume lock expired without resolution")
		case state == "running":
			return nil
		case state == "paused":
			return errors.New("peer resume left sandbox paused")
		// pausing / resuming → keep polling
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}
