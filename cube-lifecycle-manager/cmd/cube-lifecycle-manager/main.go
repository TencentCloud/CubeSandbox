// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// cube-lifecycle-manager drives the auto-pause / auto-resume loop that sits
// between CubeMaster, CubeProxy, and Redis.
package main

import (
	"context"
	"errors"
	"fmt"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/config"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/cubemasterclient"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/discovery"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/eventbus"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/httpapi"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/leader"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/lifecycle"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/proxypush"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/redisclient"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/redisstream"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/registry"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/resumer"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/statesync"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/sweeper"
)

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		zap.L().Fatal("cube-lifecycle-manager exit", zap.Error(err))
	}
}

func run() error {
	logger, err := zap.NewProduction()
	if err != nil {
		return err
	}
	defer func() { _ = logger.Sync() }()
	zap.ReplaceGlobals(logger)

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	logger.Info("cube-lifecycle-manager starting",
		zap.String("redis_addr", redisclient.DisplayAddr(cfg)),
		zap.Strings("cube_proxy_admin_urls", cfg.CubeProxyAdminURLs),
		zap.String("cubemaster_url", cfg.CubeMasterURL),
		zap.String("listen_addr", cfg.ListenAddr),
		zap.String("instance_identity", cfg.ConsumerName),
		zap.Bool("leader_election_enabled", cfg.LeaderElectionEnabled))

	rdb := redisclient.New(cfg)
	defer func() { _ = rdb.Close() }()

	stream := redisstream.New(rdb, logger.Named("redis"))
	masterClient := cubemasterclient.New(cfg.CubeMasterURL, cfg.HTTPTimeout)
	reg := registry.New()

	// The eventbus carries best-effort cross-replica wakeup hints. Redis
	// remains the source of truth and the wait path retains polling fallback.
	var bus *eventbus.Bus
	if cfg.EventBusEnabled {
		bus = eventbus.New()
		stream.SetNotifyEnabled(true)
		stream.SetLocalBus(bus)
		logger.Info("eventbus enabled")
	} else {
		logger.Info("eventbus disabled (waitForRunning will poll)")
	}

	rootCtx, cancel := signalContext()
	defer cancel()

	// startupTs marks the boundary between "bootstrap entries (HGETALL)"
	// and stream entries for the sweeper's warmup logic.
	startupTs := time.Now()

	lease := leader.New(leader.Options{
		Redis:         rdb,
		Key:           lifecycle.LeaderLeaseKey,
		Identity:      cfg.ConsumerName,
		Enabled:       cfg.LeaderElectionEnabled,
		TTL:           cfg.LeaderLeaseTTL,
		RenewInterval: cfg.LeaderRenewInterval,
		RetryInterval: cfg.LeaderRetryInterval,
		Log:           logger.Named("leader"),
	})
	activeLeader := &reconciledLeader{lease: lease}
	var eventApplyMu sync.Mutex

	// Build the CubeProxy fleet. Two sources are supported:
	//   * CUBE_LCM_PROXY_ADMIN_URLS non-empty  → static list (single-host dev)
	//   * default                              → Redis service discovery
	// The two are mutually exclusive; if the static list is set, discovery
	// is skipped entirely so the operator's intent is honored precisely.
	var (
		fleet   proxypush.Fleet
		discSvc *discovery.RedisDiscovery
	)
	if len(cfg.CubeProxyAdminURLs) > 0 && cfg.UseStaticFleet {
		fleet = discovery.NewStatic(cfg.CubeProxyAdminURLs)
		logger.Info("using static CubeProxy fleet (discovery disabled)",
			zap.Strings("admin_urls", cfg.CubeProxyAdminURLs))
	}

	// pushClient reads Fleet.Snapshot() on every call, so a later swap-in of
	// the RedisDiscovery Fleet is picked up automatically. We construct the
	// discovery instance below so its onJoin can reference pushClient.
	var pushClient *proxypush.Client

	if fleet == nil {
		discSvc = discovery.New(discovery.Options{
			Redis:           rdb,
			Log:             logger.Named("discovery"),
			HeartbeatTTL:    cfg.HeartbeatTTL,
			RefreshInterval: cfg.DiscoveryRefresh,
			OnJoin: func(ep discovery.Endpoint) {
				if !activeLeader.IsLeader() {
					return
				}
				// Replay metadata and terminal state to the newly-arrived
				// proxy. We must not block the discovery refresh loop, so
				// this runs in its own goroutine.
				go replayRegistryTo(rootCtx, pushClient, stream, reg, ep, logger.Named("replay"))
			},
			OnLeave: func(proxyID string) {
				logger.Info("proxy left; further broadcasts will skip it",
					zap.String("proxy_id", proxyID))
			},
			Leader: activeLeader,
		})
		fleet = discSvc
	}

	pushClient = proxypush.NewWithFleet(fleet, cfg.CubeAdminToken, cfg.HTTPTimeout, logger.Named("proxypush"))

	// Capture the stream cursor before HGETALL so events written during
	// bootstrap are replayed by every replica.
	streamCursor, err := stream.LatestID(rootCtx)
	if err != nil {
		return err
	}
	streamProgress := newStreamProgress(streamCursor)

	// 1. Bootstrap the in-memory registry from the meta HSet. We do NOT push
	//    entries to CubeProxy from here — the onJoin callback (or the static
	//    fleet's initial replay below) is the single point that hydrates each
	//    proxy. This keeps the "who pushes what to whom" invariant simple:
	//    every meta hits every proxy exactly through the onJoin replay + the
	//    stream consumer loop.
	if err := bootstrapRegistry(rootCtx, stream, reg, startupTs, logger); err != nil {
		return err
	}
	resumeImpl := resumer.New(resumer.Options{
		Registry:     reg,
		Redis:        stream,
		CubeMaster:   masterClient,
		ProxyPush:    pushClient,
		StateLockTTL: cfg.StateLockTTL,
		Log:          logger.Named("resumer"),
		EventBus:     bus,
	})

	sweep := sweeper.New(sweeper.Options{
		Registry:           reg,
		Redis:              stream,
		CubeMaster:         masterClient,
		ProxyPush:          pushClient,
		DefaultIdleTimeout: cfg.DefaultIdleTimeout,
		BootstrapWarmup:    cfg.BootstrapWarmup,
		StateLockTTL:       cfg.StateLockTTL,
		Interval:           cfg.IdleSweepInterval,
		StartedAt:          startupTs,
		Log:                logger.Named("sweeper"),
		Leader:             activeLeader,
	})

	apiSrv := httpapi.New(cfg.ListenAddr, resumeImpl, reg, logger.Named("http")).
		WithFleetSizer(fleetSizer{fleet}).
		WithLeaderStatus(activeLeader)

	// 3. Run all background loops concurrently. First error cancels the rest.
	loopCount := 6
	if discSvc != nil {
		loopCount++
	}
	if cfg.EventBusEnabled {
		loopCount++
	}
	stateSyncDeps := statesync.Deps{
		Registry:  reg,
		Redis:     stream,
		ProxyPush: pushClient,
		TTL:       cfg.StateLockTTL,
		Log:       logger.Named("statesync"),
		Leader:    activeLeader,
		Persister: lease,
	}

	errs := make(chan error, loopCount)
	go func() {
		errs <- consumeStream(
			rootCtx, stream, pushClient, reg, cfg,
			stateSyncDeps, activeLeader, streamProgress, &eventApplyMu, startupTs, logger.Named("stream"),
		)
	}()
	go func() { errs <- pollLastActive(rootCtx, pushClient, reg, cfg.LastActivePoll, logger.Named("active")) }()
	go func() { errs <- sweep.Run(rootCtx) }()
	go func() { errs <- apiSrv.Run(rootCtx) }()
	go func() { errs <- lease.Run(rootCtx) }()
	go func() {
		errs <- reconcileOnLeadership(
			rootCtx, lease, activeLeader, stream, pushClient, reg, fleet,
			stateSyncDeps, streamProgress, &eventApplyMu,
			cfg.LeaderRetryInterval, cfg.HTTPTimeout, logger.Named("promotion"),
		)
	}()
	if discSvc != nil {
		go func() { errs <- discSvc.Run(rootCtx) }()
	} else if !cfg.LeaderElectionEnabled {
		// Static fleet has no OnJoin; with election disabled there is also no
		// promotion hydrate, so push the snapshot once at startup.
		go hydrateFleet(rootCtx, pushClient, stream, reg, fleet, activeLeader, logger.Named("replay"))
	}
	if cfg.EventBusEnabled {
		sub := eventbus.NewSubscriber(rdb, bus, logger.Named("eventbus"))
		go func() { errs <- sub.Run(rootCtx) }()
	}

	// First loop to return wins; we cancel siblings via context and drain.
	first := <-errs
	cancel()
	for i := 0; i < loopCount-1; i++ {
		<-errs
	}
	return first
}

// reconciledLeader becomes executable only after the current lease generation
// has caught up the event stream and drained in-flight CubeProxy writes from
// a previous leader. Election-disabled deployments skip that barrier.
type reconciledLeader struct {
	lease      *leader.Lease
	generation atomic.Uint64
	ready      atomic.Bool
}

func (s *reconciledLeader) IsLeader() bool {
	if !s.lease.Enabled() {
		return s.lease.IsLeader()
	}
	generation := s.lease.Generation()
	return generation != 0 &&
		s.ready.Load() &&
		s.generation.Load() == generation &&
		s.lease.IsLeader()
}

func (s *reconciledLeader) Enabled() bool { return s.lease.Enabled() }

func (s *reconciledLeader) markReconciled(generation uint64) {
	s.generation.Store(generation)
	s.ready.Store(true)
}

// invalidate pauses singleton work without forgetting the drained generation.
// Transient XREAD errors must not call this; only a rebuilt-from-trim registry
// is unsafe to sweep.
func (s *reconciledLeader) invalidate() {
	s.ready.Store(false)
}

func (s *reconciledLeader) restoreIfSameGeneration() {
	generation := s.lease.Generation()
	if generation != 0 && generation == s.generation.Load() && s.lease.IsLeader() {
		s.ready.Store(true)
	}
}

type streamProgress struct {
	mu     sync.RWMutex
	cursor string
}

func newStreamProgress(cursor string) *streamProgress {
	return &streamProgress{cursor: cursor}
}

func (p *streamProgress) Cursor() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cursor
}

func (p *streamProgress) Advance(cursor string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cmp, err := redisstream.CompareStreamIDs(cursor, p.cursor)
	if err == nil && cmp > 0 {
		p.cursor = cursor
	}
}

func (p *streamProgress) ShouldApply(cursor string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	cmp, err := redisstream.CompareStreamIDs(cursor, p.cursor)
	return err == nil && cmp > 0
}

func (p *streamProgress) Reset(cursor string) {
	p.mu.Lock()
	p.cursor = cursor
	p.mu.Unlock()
}

// fleetSizer adapts a proxypush.Fleet to httpapi.FleetSizer so /readyz can
// surface the current live-replica count without pulling discovery into the
// httpapi package.
type fleetSizer struct {
	f proxypush.Fleet
}

func (s fleetSizer) Snapshot() int {
	if s.f == nil {
		return 0
	}
	return len(s.f.Snapshot())
}

// replayRegistryTo pushes metadata and terminal state to a single admin
// endpoint. Used by discovery.OnJoin and best-effort fleet hydration.
func replayRegistryTo(ctx context.Context, push *proxypush.Client, stream *redisstream.Client,
	reg *registry.Registry, ep discovery.Endpoint, log *zap.Logger) bool {

	entries := reg.Snapshot()
	log.Info("replay begin",
		zap.String("proxy_id", ep.ProxyID),
		zap.String("admin_url", ep.AdminURL),
		zap.Int("entries", len(entries)))
	var pushed, failed int
	for _, e := range entries {
		if ctx.Err() != nil {
			return false
		}
		if err := push.UpsertMetaTo(ctx, ep.AdminURL, e.Meta); err != nil {
			failed++
			log.Warn("replay push failed",
				zap.String("proxy_id", ep.ProxyID),
				zap.String("sandbox_id", e.Meta.SandboxID), zap.Error(err))
			continue
		}
		state, err := resolvePromotionState(ctx, stream, e)
		if err != nil {
			failed++
			log.Warn("replay state read failed",
				zap.String("proxy_id", ep.ProxyID),
				zap.String("sandbox_id", e.Meta.SandboxID), zap.Error(err))
			continue
		}
		if state == lifecycle.StatePaused || state == lifecycle.StateRunning {
			if err := push.SetStateTo(ctx, ep.AdminURL, e.Meta.SandboxID, state); err != nil {
				failed++
				log.Warn("replay state push failed",
					zap.String("proxy_id", ep.ProxyID),
					zap.String("sandbox_id", e.Meta.SandboxID), zap.Error(err))
				continue
			}
		}
		pushed++
	}
	log.Info("replay done",
		zap.String("proxy_id", ep.ProxyID),
		zap.Int("pushed", pushed), zap.Int("failed", failed))
	return failed == 0
}

func hydrateFleet(ctx context.Context, push *proxypush.Client, stream *redisstream.Client,
	reg *registry.Registry, fleet proxypush.Fleet, active leader.Status, log *zap.Logger) {

	if fleet == nil {
		return
	}
	for _, ep := range fleet.Snapshot() {
		if ctx.Err() != nil {
			return
		}
		if active != nil && !active.IsLeader() {
			return
		}
		replayRegistryTo(ctx, push, stream, reg, ep, log)
	}
}

// reconcileOnLeadership catches the newly promoted replica up to the stream
// high-water, waits one CubeProxy HTTP timeout so in-flight writes from the
// previous leader can finish, catches up again, then allows singleton work.
// The HTTP drain runs only when the lease generation changed; a same-generation
// restore (stream trim) must not stall singleton work. Catch-up CAS's terminal
// state into the shared Redis key (lease holder) but does not push CubeProxy;
// fleet hydration is best-effort afterwards and must not gate leadership.
func reconcileOnLeadership(ctx context.Context, lease *leader.Lease, active *reconciledLeader,
	stream *redisstream.Client, push *proxypush.Client, reg *registry.Registry,
	fleet proxypush.Fleet, ssDeps statesync.Deps, progress *streamProgress,
	eventApplyMu *sync.Mutex, interval, drain time.Duration, log *zap.Logger) error {

	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		generation := lease.Generation()
		if generation != 0 && lease.IsLeader() && !active.IsLeader() {
			prev := active.generation.Load()
			needDrain := generation != prev
			log.Info("leadership catch-up begin",
				zap.Uint64("generation", generation),
				zap.Bool("drain", needDrain && drain > 0))
			caughtUp := catchUpGeneration(ctx, generation, lease, stream, push, reg, ssDeps, progress, eventApplyMu, log)
			if caughtUp && needDrain && drain > 0 {
				if !waitForRetry(ctx, drain) {
					return ctx.Err()
				}
				caughtUp = catchUpGeneration(ctx, generation, lease, stream, push, reg, ssDeps, progress, eventApplyMu, log)
			}
			if caughtUp && lease.IsLeader() && generation == lease.Generation() {
				active.markReconciled(generation)
				log.Info("leadership catch-up complete", zap.Uint64("generation", generation))
				if needDrain {
					go hydrateFleet(ctx, push, stream, reg, fleet, active, log)
				}
			} else if lease.IsLeader() {
				log.Warn("leadership catch-up incomplete; retrying",
					zap.Uint64("generation", generation))
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func catchUpGeneration(ctx context.Context, generation uint64, lease *leader.Lease,
	stream *redisstream.Client, push *proxypush.Client, reg *registry.Registry,
	ssDeps statesync.Deps, progress *streamProgress, eventApplyMu *sync.Mutex,
	log *zap.Logger) bool {

	eventApplyMu.Lock()
	defer eventApplyMu.Unlock()
	if !lease.IsLeader() || generation != lease.Generation() {
		return false
	}
	target, err := stream.LatestID(ctx)
	if err != nil {
		log.Warn("promotion high-water read failed", zap.Error(err))
		return false
	}
	if err := catchUpStreamTo(ctx, target, stream, push, reg, ssDeps, progress, log); err != nil {
		log.Warn("promotion stream catch-up failed", zap.Error(err))
		return false
	}
	return true
}

func resolvePromotionState(
	ctx context.Context, stream *redisstream.Client, entry registry.Entry,
) (string, error) {
	// After promotion catch-up the lease holder has CAS'd terminal state
	// events into this key, so it is at least as fresh as RuntimeState.
	state, ok, err := stream.GetState(ctx, entry.Meta.SandboxID)
	if err != nil {
		return "", err
	}
	if ok {
		return state, nil
	}
	return entry.RuntimeState, nil
}

func catchUpStreamTo(ctx context.Context, target string, stream *redisstream.Client,
	push *proxypush.Client, reg *registry.Registry, ssDeps statesync.Deps,
	progress *streamProgress, log *zap.Logger) error {

	for {
		cursor := progress.Cursor()
		cmp, err := redisstream.CompareStreamIDs(cursor, target)
		if err != nil {
			return err
		}
		if cmp >= 0 {
			return nil
		}
		events, next, err := stream.Read(ctx, cursor, -1, 100)
		if err != nil {
			return err
		}
		if next == cursor {
			return fmt.Errorf("stream catch-up stalled at %s before %s", cursor, target)
		}
		for _, ev := range events {
			handleEvent(ctx, ev, push, reg, ssDeps, log)
			progress.Advance(ev.StreamID)
		}
		// Malformed entries are intentionally omitted from events but still
		// advance Read's cursor.
		progress.Advance(next)
	}
}

func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	return ctx, cancel
}

// bootstrapRegistry reads the meta HSet and hydrates the in-memory registry.
// It does NOT push to CubeProxy: fleet hydration is performed after leadership
// reconciliation, or by discovery.OnJoin for a later proxy arrival.
//
// Bootstrap entries get their FirstSeenAt backdated to a fixed startup
// timestamp so the sweeper's BootstrapWarmup gate can distinguish "loaded
// from HGETALL at process start" (FirstSeenAt == startupTs) from "arrived
// later via stream" (FirstSeenAt > startupTs).
func bootstrapRegistry(ctx context.Context, stream *redisstream.Client,
	reg *registry.Registry, startupTs time.Time, log *zap.Logger) error {

	metas, err := stream.Bootstrap(ctx)
	if err != nil {
		return err
	}
	reg.Reset()
	for _, m := range metas {
		reg.Upsert(m)
		reg.SetFirstSeenAt(m.SandboxID, startupTs)
	}
	log.Info("bootstrap complete", zap.Int("entries", len(metas)))
	return nil
}

// consumeStream is the increment-side of the lifecycle channel. It maintains
// the registry + pushes deltas to CubeProxy as create / delete events arrive.
func consumeStream(ctx context.Context, stream *redisstream.Client, push *proxypush.Client,
	reg *registry.Registry, cfg *config.Config, ssDeps statesync.Deps,
	active *reconciledLeader, progress *streamProgress, eventApplyMu *sync.Mutex,
	startupTs time.Time, log *zap.Logger) error {

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		cursor := progress.Cursor()
		events, nextCursor, err := stream.Read(ctx, cursor, cfg.StreamReadBlock, 100)
		if errors.Is(err, redisstream.ErrCursorTrimmed) {
			active.invalidate()
			eventApplyMu.Lock()
			rebuildErr := rebuildRegistryAfterTrim(ctx, stream, reg, progress, startupTs, log)
			eventApplyMu.Unlock()
			if rebuildErr != nil {
				log.Warn("stream gap reconciliation failed; backing off", zap.Error(rebuildErr))
				if !waitForRetry(ctx, time.Second) {
					return ctx.Err()
				}
				continue
			}
			active.restoreIfSameGeneration()
			log.Warn("stream cursor was trimmed; registry rebuilt",
				zap.String("old_cursor", cursor),
				zap.String("new_cursor", progress.Cursor()))
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Warn("xread failed; backing off", zap.Error(err))
			if !waitForRetry(ctx, time.Second) {
				return ctx.Err()
			}
			continue
		}
		for _, ev := range events {
			eventApplyMu.Lock()
			if !progress.ShouldApply(ev.StreamID) {
				eventApplyMu.Unlock()
				continue
			}
			handleEvent(ctx, ev, push, reg, ssDeps, log)
			progress.Advance(ev.StreamID)
			eventApplyMu.Unlock()
		}
		progress.Advance(nextCursor)
	}
}

// rebuildRegistryAfterTrim reloads the Hash snapshot after MAXLEN has dropped
// events. LatestID is captured before HGETALL so the consumer does not skip
// events CubeMaster wrote between the two reads (Hash then Stream, not a
// transaction). Local LastActiveMs / RuntimeState are preserved.
func rebuildRegistryAfterTrim(ctx context.Context, stream *redisstream.Client,
	reg *registry.Registry, progress *streamProgress, startupTs time.Time, log *zap.Logger) error {

	type preserved struct {
		lastActive int64
		runtime    string
	}
	prev := make(map[string]preserved, reg.Len())
	for _, e := range reg.Snapshot() {
		prev[e.Meta.SandboxID] = preserved{lastActive: e.LastActiveMs, runtime: e.RuntimeState}
	}

	cursor, err := stream.LatestID(ctx)
	if err != nil {
		return err
	}
	if err := bootstrapRegistry(ctx, stream, reg, startupTs, log); err != nil {
		return err
	}
	for sid, p := range prev {
		if p.lastActive > 0 {
			reg.MergeLastActive(sid, p.lastActive)
		}
		if p.runtime != "" {
			reg.SetRuntimeState(sid, p.runtime)
		}
	}
	progress.Reset(cursor)
	return nil
}

func waitForRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func handleEvent(ctx context.Context, ev redisstream.Event, push *proxypush.Client,
	reg *registry.Registry, ssDeps statesync.Deps, log *zap.Logger) {

	canWrite := func() bool {
		return ssDeps.Leader == nil || ssDeps.Leader.IsLeader()
	}
	switch ev.Op {
	case lifecycle.OpCreate:
		if ev.Meta == nil {
			log.Warn("create event missing payload",
				zap.String("sandbox_id", ev.SandboxID))
			return
		}
		reg.Upsert(*ev.Meta)
		// Log every create at info level: this is the heartbeat that
		// proves CubeMaster -> Redis -> CLM is wired correctly. The
		// volume is bounded by sandbox creation rate (≪ QPS) so this is
		// not a noise concern.
		log.Info("create event applied",
			zap.String("sandbox_id", ev.SandboxID),
			zap.Bool("auto_pause", ev.Meta.AutoPause),
			zap.Bool("auto_resume", ev.Meta.AutoResume),
			zap.Intp("timeout_seconds", ev.Meta.TimeoutSeconds),
			zap.Int("registry_size", reg.Len()))
		if canWrite() {
			if err := push.UpsertMeta(ctx, *ev.Meta); err != nil {
				log.Warn("create event push failed",
					zap.String("sandbox_id", ev.SandboxID), zap.Error(err))
			}
		}
	case lifecycle.OpDelete:
		reg.Delete(ev.SandboxID)
		log.Info("delete event applied",
			zap.String("sandbox_id", ev.SandboxID),
			zap.Int("registry_size", reg.Len()))
		if canWrite() {
			if err := push.DeleteMeta(ctx, ev.SandboxID); err != nil {
				log.Warn("delete event push failed",
					zap.String("sandbox_id", ev.SandboxID), zap.Error(err))
			}
		}
	case lifecycle.OpUpdate:
		if ev.Meta == nil {
			log.Warn("update event missing payload",
				zap.String("sandbox_id", ev.SandboxID))
			return
		}
		reg.Upsert(*ev.Meta)
		reg.ResetLastActive(ev.SandboxID)
		log.Info("update event applied",
			zap.String("sandbox_id", ev.SandboxID),
			zap.Bool("auto_pause", ev.Meta.AutoPause),
			zap.Bool("auto_resume", ev.Meta.AutoResume),
			zap.Intp("timeout_seconds", ev.Meta.TimeoutSeconds),
			zap.Int64("created_at_ms", ev.Meta.CreatedAt),
			zap.Int64("end_at_ms", ev.Meta.EndAt))
		if canWrite() {
			if err := push.UpsertMeta(ctx, *ev.Meta); err != nil {
				log.Warn("update event push failed",
					zap.String("sandbox_id", ev.SandboxID), zap.Error(err))
			}
		}
	case lifecycle.OpState:
		// Reconcile externally-driven pause/resume (e.g. SDK connect())
		// against the CLM's Redis state key + CubeProxy dict.
		statesync.Handle(ctx, ssDeps, ev)
	default:
		log.Warn("unknown event op",
			zap.String("op", ev.Op),
			zap.String("sandbox_id", ev.SandboxID))
	}
}

// pollLastActive pulls /admin/last_active from every CubeProxy and merges
// the timestamps into the registry. The sweeper consumes the merged view.
func pollLastActive(ctx context.Context, push *proxypush.Client, reg *registry.Registry,
	interval time.Duration, log *zap.Logger) error {

	t := time.NewTicker(interval)
	defer t.Stop()

	var since int64
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
		entries, minNow, err := push.PullLastActive(ctx, since)
		if err != nil {
			log.Warn("pull last_active failed", zap.Error(err))
			continue
		}
		for sid, ts := range entries {
			reg.MergeLastActive(sid, ts)
		}
		// Bump the watermark so the next pull is incremental. Using the
		// minimum `now` across responses guarantees no entry can fall into
		// the (since, next_since] gap if one CubeProxy clock is behind.
		if minNow > since {
			since = minNow
		}
	}
}
