// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package reconciler drives the CLM's view of the world back to the
// authoritative lifecycle meta hash on a fixed cadence (issue #1211). The
// stream consumer keeps the registry current incrementally, but events can
// be missed across leader failovers, Redis blips, or failed proxy pushes;
// this loop is the eventual-consistency backstop:
//
//   - sandbox in the meta hash but not in the registry   → adopt it and push
//     its meta to every CubeProxy (covers events lost while no leader was
//     running, and pushes that failed on the old leader);
//   - sandbox in both but with a stale meta              → refresh and
//     re-push (covers a lost update event);
//   - sandbox in the registry but gone from the hash     → evict it from the
//     registry, the proxies, and the per-sandbox state key (covers a lost
//     delete event);
//
// CubeMaster always writes the meta hash before appending to the events
// stream (HSET/HDEL precede XADD, see CubeMaster/pkg/lifecycle/store.go), so
// the hash is never staler than anything the stream consumer has applied.
package reconciler

import (
	"context"
	"reflect"
	"time"

	"go.uber.org/zap"

	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/lifecycle"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/registry"
)

// metaStore is the subset of redisstream.Client the reconciler needs.
type metaStore interface {
	Bootstrap(ctx context.Context) (map[string]lifecycle.SandboxLifecycleMeta, error)
	ClearState(ctx context.Context, sandboxID string) error
}

// metaNotifier is the subset of proxypush.Client the reconciler needs.
type metaNotifier interface {
	UpsertMeta(ctx context.Context, meta lifecycle.SandboxLifecycleMeta) error
	DeleteMeta(ctx context.Context, sandboxID string) error
}

// Options bundles the reconciler's dependencies; tests substitute fakes.
type Options struct {
	Registry  *registry.Registry
	Redis     metaStore
	ProxyPush metaNotifier
	// Interval between reconcile passes.
	Interval time.Duration
	Now      func() time.Time // injectable for tests
	Log      *zap.Logger
}

// Reconciler runs one diff-and-converge pass per Interval. It is intended
// to run on the leader only, as its own goroutine; Run returns when ctx is
// cancelled.
type Reconciler struct {
	o Options
}

func New(o Options) *Reconciler {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = zap.NewNop()
	}
	return &Reconciler{o: o}
}

// Run blocks until ctx is cancelled, reconciling every Interval. The first
// pass fires on the first tick — leadership acquisition already performs a
// full bootstrap, so there is no value in a zeroth immediate pass.
func (r *Reconciler) Run(ctx context.Context) error {
	t := time.NewTicker(r.o.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			r.reconcileOnce(ctx)
		}
	}
}

func (r *Reconciler) reconcileOnce(ctx context.Context) {
	metas, err := r.o.Redis.Bootstrap(ctx)
	if err != nil {
		r.o.Log.Warn("reconcile: meta hash read failed; skipping pass", zap.Error(err))
		return
	}

	var adopted, updated, evicted int

	seen := make(map[string]struct{}, len(metas))
	for sid, meta := range metas {
		seen[sid] = struct{}{}
		cur := r.o.Registry.Get(sid)
		switch {
		case cur == nil:
			r.o.Registry.Upsert(meta)
			r.pushUpsert(ctx, sid, meta)
			adopted++
		case !reflect.DeepEqual(cur.Meta, meta):
			r.o.Registry.Upsert(meta)
			// Mirror the stream OpUpdate path (main.go handleEvent): an
			// update refreshes the activity baseline, otherwise an extended
			// TimeoutSeconds applied only through this loop — e.g. the
			// update event was lost in a failover — leaves the old
			// LastActiveMs in place and the sweeper can pause the sandbox
			// on the next pass.
			r.o.Registry.ResetLastActive(sid)
			r.pushUpsert(ctx, sid, meta)
			updated++
		}
	}

	for _, e := range r.o.Registry.Snapshot() {
		sid := e.Meta.SandboxID
		if _, ok := seen[sid]; ok {
			continue
		}
		// Grace window: skip entries we learned about less than one Interval
		// ago. The hash is never staler than the stream (see package doc),
		// so this is pure belt-and-braces against out-of-order writers.
		if r.o.Now().Sub(e.FirstSeenAt) < r.o.Interval {
			continue
		}
		r.o.Registry.Delete(sid)
		if err := r.o.ProxyPush.DeleteMeta(ctx, sid); err != nil {
			r.o.Log.Warn("reconcile: delete push failed",
				zap.String("sandbox_id", sid), zap.Error(err))
		}
		// A state key could linger from an interrupted transition on the old
		// leader; the sandbox is gone, so drop it. (Keys also TTL out on
		// their own — this just shortens the window.)
		if err := r.o.Redis.ClearState(ctx, sid); err != nil {
			r.o.Log.Warn("reconcile: clear state failed",
				zap.String("sandbox_id", sid), zap.Error(err))
		}
		evicted++
	}

	if adopted+updated+evicted > 0 {
		r.o.Log.Info("reconcile pass applied drift",
			zap.Int("adopted", adopted),
			zap.Int("updated", updated),
			zap.Int("evicted", evicted),
			zap.Int("registry_size", r.o.Registry.Len()))
	}
}

func (r *Reconciler) pushUpsert(ctx context.Context, sid string, meta lifecycle.SandboxLifecycleMeta) {
	if err := r.o.ProxyPush.UpsertMeta(ctx, meta); err != nil {
		r.o.Log.Warn("reconcile: meta push failed",
			zap.String("sandbox_id", sid), zap.Error(err))
	}
}
