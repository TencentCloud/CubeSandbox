// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// cube-lifecycle-manager drives the auto-pause / auto-resume loop that sits
// between CubeMaster, CubeProxy, and Redis. It supersedes the older
// in-container "cube-proxy-sidecar"; the wire protocol with CubeProxy
// (admin push endpoints + /_sidecar_resume callback) is unchanged.
//
// Active-standby HA (issue #1211): when CUBE_LCM_HA_ENABLED=1, replicas
// elect a leader through a Redis lease (internal/leaderelect). Only the
// leader runs the stateful loops (stream consumer, sweeper, last-active
// poller, reconciler); standbys keep the HTTP server up — gated to 503 on
// /readyz so the Service routes resume traffic to the leader — and can
// still serve /internal/resume through the meta-hash fallback. A crashed
// leader is replaced within one leader TTL; the new leader bootstraps from
// the meta hash, claims the dead consumer's pending stream entries, and
// the reconciler converges whatever drift remains.
package main

import (
	"context"
	"errors"
	"fmt"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/config"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/cubemasterclient"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/discovery"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/eventbus"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/httpapi"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/leaderelect"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/lifecycle"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/proxypush"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/reconciler"
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
		zap.String("consumer_group", cfg.ConsumerGroup),
		zap.String("consumer_name", cfg.ConsumerName),
		zap.Bool("ha_enabled", cfg.HAEnabled),
		zap.String("instance_id", cfg.InstanceID))

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

	// In active-standby mode the elector arbitrates which replica runs the
	// leader loops. Construct it before discovery so the onJoin replay can
	// consult leadership.
	var elector *leaderelect.Elector
	if cfg.HAEnabled {
		elector, err = leaderelect.New(rdb, leaderelect.Config{
			Key:           cfg.LeaderKey,
			InstanceID:    cfg.InstanceID,
			TTL:           cfg.LeaderTTL,
			RenewInterval: cfg.LeaderRenewInterval,
		}, logger.Named("leader"))
		if err != nil {
			return err
		}
	}

	// Build the CubeProxy fleet. Two sources are supported:
	//   * CUBE_LCM_PROXY_ADMIN_URLS non-empty  → static list (single-host dev)
	//   * default                              → Redis service discovery
	// The two are mutually exclusive; if the static list is set, discovery
	// is skipped entirely so the operator's intent is honored precisely.
	var (
		fleet       proxypush.Fleet
		discSvc     *discovery.RedisDiscovery
		staticFleet *discovery.Static
	)
	if len(cfg.CubeProxyAdminURLs) > 0 && cfg.UseStaticFleet {
		staticFleet = discovery.NewStatic(cfg.CubeProxyAdminURLs)
		fleet = staticFleet
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
				// A standby must not replay: its registry is empty or stale,
				// and the leader's own replay already delivers the fresh
				// snapshot. (Discovery itself keeps running on standbys so
				// the resume path's state pushes have a populated fleet.)
				if elector != nil && !elector.IsLeader() {
					return
				}
				// Replay the current registry snapshot to the newly-arrived
				// proxy. We must not block the discovery refresh loop, so
				// this runs in its own goroutine with a bounded context.
				go replayRegistryTo(rootCtx, pushClient, reg, ep, logger.Named("replay"))
			},
			OnLeave: func(proxyID string) {
				logger.Info("proxy left; further broadcasts will skip it",
					zap.String("proxy_id", proxyID))
			},
		})
		fleet = discSvc
	}

	pushClient = proxypush.NewWithFleet(fleet, cfg.CubeAdminToken, cfg.HTTPTimeout, logger.Named("proxypush"))

	resumeImpl := resumer.New(resumer.Options{
		Registry:     reg,
		Redis:        stream,
		CubeMaster:   masterClient,
		ProxyPush:    pushClient,
		StateLockTTL: cfg.StateLockTTL,
		// MetaLookup lets a standby (or a freshly promoted leader whose
		// bootstrap hasn't landed yet) serve resume requests straight from
		// the authoritative meta hash.
		MetaLookup: stream,
		Log:        logger.Named("resumer"),
		EventBus:   bus,
	})

	apiSrv := httpapi.New(cfg.ListenAddr, resumeImpl, reg, logger.Named("http")).
		WithFleetSizer(fleetSizer{fleet})
	if elector != nil {
		apiSrv = apiSrv.WithLeaderGate(elector)
	}

	// Loops that run on every replica: the HTTP server (standbys answer
	// /healthz and can serve /internal/resume via the meta fallback) and,
	// when enabled, proxy discovery. The eventbus subscriber also runs on
	// every replica so a standby's waitForRunning still receives the
	// cross-replica wakeup hints published by the leader. The leader-only
	// loops either run directly (single-replica mode) or under the elector.
	loopCount := 2
	if discSvc != nil {
		loopCount++
	}
	if cfg.EventBusEnabled {
		loopCount++
	}
	errs := make(chan error, loopCount)

	go func() { errs <- apiSrv.Run(rootCtx) }()
	if discSvc != nil {
		go func() { errs <- discSvc.Run(rootCtx) }()
	}
	if cfg.EventBusEnabled {
		sub := eventbus.NewSubscriber(rdb, bus, logger.Named("eventbus"))
		go func() { errs <- sub.Run(rootCtx) }()
	}

	leaderRun := func(ctx context.Context) error {
		return runLeaderLoops(ctx, cfg, stream, masterClient, pushClient, reg, fleet, logger)
	}
	if elector != nil {
		// A leader stint that keeps failing fast (e.g. a bootstrap
		// dependency is down) would otherwise loop elect → fail → step
		// down forever without the process ever exiting; past
		// maxLeaderStintFails consecutive fast failures we give up and let
		// the pod supervisor restart us. A stint that survived at least
		// one leader TTL counts as healthy and resets the counter.
		supervisor := newLeaderSupervisor(maxLeaderStintFails, cfg.LeaderTTL)
		go func() {
			errs <- elector.Run(rootCtx, leaderelect.Callbacks{
				OnElected: func(leaderCtx context.Context) {
					start := time.Now()
					err := leaderRun(leaderCtx)
					if supervisor.Record(err, time.Since(start)) {
						logger.Error("leader loop failed repeatedly; exiting so the pod supervisor restarts the process",
							zap.Int("consecutive_failures", supervisor.Fails()),
							zap.Error(err))
						errs <- fmt.Errorf("leader loop failed %d consecutive times: %w",
							supervisor.Fails(), err)
						return
					}
					if err != nil && !errors.Is(err, context.Canceled) {
						logger.Error("leader loop failed; stepping down so a standby can take over",
							zap.Error(err))
						elector.StepDown()
					}
				},
				OnLost: func() {
					// Drop the view built while leader: a standby answers
					// resume requests via the meta-hash fallback and must
					// never consult (or replay) a frozen snapshot.
					reg.Reset()
				},
			})
		}()
	} else {
		go func() { errs <- leaderRun(rootCtx) }()
	}

	// First loop to return wins; we cancel siblings via context and drain.
	first := <-errs
	cancel()
	for i := 0; i < loopCount-1; i++ {
		<-errs
	}
	return first
}

// runLeaderLoops executes every leader-only responsibility: registry
// bootstrap + fleet hydration, stream consumption, stale-pending-entry
// claims, last-active polling, the idle sweeper, and the periodic
// reconciler. It blocks until ctx is cancelled (leadership lost, or
// shutdown) or a loop fails; the first error wins and cancels the rest.
func runLeaderLoops(ctx context.Context, cfg *config.Config, stream *redisstream.Client,
	masterClient *cubemasterclient.Client, pushClient *proxypush.Client, reg *registry.Registry,
	fleet proxypush.Fleet, logger *zap.Logger) error {

	// startupTs marks the boundary between "bootstrap entries (HGETALL)"
	// and "stream entries (XREADGROUP)" for the sweeper's warmup logic. It
	// re-anchors on every (re-)election so a promoted standby gets the same
	// warmup protection as a freshly started process.
	startupTs := time.Now()

	// 1. Bootstrap the in-memory registry from the meta HSet, then hydrate
	//    every currently-known proxy with the snapshot. This keeps the "who
	//    pushes what to whom" invariant simple: every meta hits every proxy
	//    exactly through the bootstrap replay (or discovery's onJoin for
	//    later arrivals) + the stream consumer loop.
	if err := bootstrapRegistry(ctx, stream, reg, startupTs, logger); err != nil {
		return err
	}
	for _, ep := range fleet.Snapshot() {
		replayRegistryTo(ctx, pushClient, reg, ep, logger.Named("replay"))
	}

	// 2. Ensure the consumer group exists.
	if err := stream.EnsureGroup(ctx, cfg.ConsumerGroup); err != nil {
		return err
	}

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
	})

	recon := reconciler.New(reconciler.Options{
		Registry:  reg,
		Redis:     stream,
		ProxyPush: pushClient,
		Interval:  cfg.ReconcileInterval,
		Log:       logger.Named("reconciler"),
	})

	stateSyncDeps := statesync.Deps{
		Registry:  reg,
		Redis:     stream,
		ProxyPush: pushClient,
		TTL:       cfg.StateLockTTL,
		Log:       logger.Named("statesync"),
	}

	// 3. Run all leader loops concurrently. First error cancels the rest.
	const loopCount = 5
	errs := make(chan error, loopCount)
	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		errs <- consumeStream(loopCtx, stream, pushClient, reg, cfg, stateSyncDeps, logger.Named("stream"))
	}()
	go func() {
		errs <- claimStalePending(loopCtx, stream, pushClient, reg, cfg, stateSyncDeps, logger.Named("claim"))
	}()
	go func() { errs <- pollLastActive(loopCtx, pushClient, reg, cfg.LastActivePoll, logger.Named("active")) }()
	go func() { errs <- sweep.Run(loopCtx) }()
	go func() { errs <- recon.Run(loopCtx) }()

	first := <-errs
	cancel()
	for i := 0; i < loopCount-1; i++ {
		<-errs
	}
	return first
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

// replayRegistryTo pushes every current registry entry to a single admin
// endpoint. Used by discovery.OnJoin (when a new CubeProxy arrives) and by
// the leadership bootstrap to hydrate the fleet. Errors are logged but not
// escalated: reconciliation eventually converges via the stream consumer
// and the periodic reconciler.
func replayRegistryTo(ctx context.Context, push *proxypush.Client,
	reg *registry.Registry, ep discovery.Endpoint, log *zap.Logger) {

	entries := reg.Snapshot()
	log.Info("replay begin",
		zap.String("proxy_id", ep.ProxyID),
		zap.String("admin_url", ep.AdminURL),
		zap.Int("entries", len(entries)))
	var pushed, failed int
	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}
		if err := push.UpsertMetaTo(ctx, ep.AdminURL, e.Meta); err != nil {
			failed++
			log.Warn("replay push failed",
				zap.String("proxy_id", ep.ProxyID),
				zap.String("sandbox_id", e.Meta.SandboxID), zap.Error(err))
			continue
		}
		pushed++
	}
	log.Info("replay done",
		zap.String("proxy_id", ep.ProxyID),
		zap.Int("pushed", pushed), zap.Int("failed", failed))
}

func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	return ctx, cancel
}

// bootstrapRegistry reads the meta HSet and hydrates the in-memory registry.
// It does NOT push to CubeProxy on its own: fleet hydration happens through
// the replay calls in runLeaderLoops / discovery.OnJoin. Keeping registry
// seeding and admin pushes separate simplifies the invariant "every meta
// reaches every proxy through bootstrap replay + onJoin + stream".
//
// Bootstrap entries get their FirstSeenAt backdated to a fixed startup
// timestamp so the sweeper's BootstrapWarmup gate can distinguish "loaded
// from HGETALL at (re-)election" (FirstSeenAt == startupTs) from "arrived
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
	reg *registry.Registry, cfg *config.Config, ssDeps statesync.Deps, log *zap.Logger) error {

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		events, err := stream.ReadGroup(ctx, cfg.ConsumerGroup, cfg.ConsumerName,
			cfg.StreamReadBlock, 100)
		if err != nil {
			log.Warn("xreadgroup failed; backing off", zap.Error(err))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
			continue
		}
		for _, ev := range events {
			handleEvent(ctx, ev, push, reg, ssDeps, log)
			if err := stream.Ack(ctx, cfg.ConsumerGroup, ev.StreamID); err != nil {
				log.Warn("ack failed",
					zap.String("id", ev.StreamID), zap.Error(err))
			}
		}
	}
}

// claimStalePending is the failover half of stream consumption: it
// periodically takes over entries that were delivered to a consumer which
// has since died or been demoted (in HA mode, typically the previous
// leader) and feeds them through the same handler as live deliveries.
// Without it those events would sit in the pending-entries list until the
// stream's MAXLEN trim drops them, leaving registry/proxy state diverged.
func claimStalePending(ctx context.Context, stream *redisstream.Client, push *proxypush.Client,
	reg *registry.Registry, cfg *config.Config, ssDeps statesync.Deps, log *zap.Logger) error {

	t := time.NewTicker(cfg.ReconcileInterval)
	defer t.Stop()

	for {
		// minIdle == ReconcileInterval: comfortably longer than a slow
		// handleEvent batch, so a live consumer's in-flight entries are
		// never stolen, while a dead leader's leftovers are taken over
		// within one reconcile interval of a promotion.
		events, err := stream.ClaimPending(ctx, cfg.ConsumerGroup, cfg.ConsumerName,
			cfg.ReconcileInterval, 100)
		if err != nil {
			log.Warn("xautoclaim failed; retrying next tick", zap.Error(err))
		} else {
			for _, ev := range events {
				handleEvent(ctx, ev, push, reg, ssDeps, log)
				if err := stream.Ack(ctx, cfg.ConsumerGroup, ev.StreamID); err != nil {
					log.Warn("ack claimed event failed",
						zap.String("id", ev.StreamID), zap.Error(err))
				}
			}
			if len(events) > 0 {
				log.Info("claimed stale pending events", zap.Int("count", len(events)))
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func handleEvent(ctx context.Context, ev redisstream.Event, push *proxypush.Client,
	reg *registry.Registry, ssDeps statesync.Deps, log *zap.Logger) {

	switch ev.Op {
	case lifecycle.OpCreate:
		if ev.Meta == nil {
			log.Warn("create event missing payload",
				zap.String("sandbox_id", ev.SandboxID))
			return
		}
		reg.Upsert(*ev.Meta)
		// Log every create at info level: this is the heartbeat that
		// proves CubeMaster -> Redis -> sidecar is wired correctly. The
		// volume is bounded by sandbox creation rate (≪ QPS) so this is
		// not a noise concern.
		log.Info("create event applied",
			zap.String("sandbox_id", ev.SandboxID),
			zap.Bool("auto_pause", ev.Meta.AutoPause),
			zap.Bool("auto_resume", ev.Meta.AutoResume),
			zap.Intp("timeout_seconds", ev.Meta.TimeoutSeconds),
			zap.Int("registry_size", reg.Len()))
		if err := push.UpsertMeta(ctx, *ev.Meta); err != nil {
			log.Warn("create event push failed",
				zap.String("sandbox_id", ev.SandboxID), zap.Error(err))
		}
	case lifecycle.OpDelete:
		reg.Delete(ev.SandboxID)
		log.Info("delete event applied",
			zap.String("sandbox_id", ev.SandboxID),
			zap.Int("registry_size", reg.Len()))
		if err := push.DeleteMeta(ctx, ev.SandboxID); err != nil {
			log.Warn("delete event push failed",
				zap.String("sandbox_id", ev.SandboxID), zap.Error(err))
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
		if err := push.UpsertMeta(ctx, *ev.Meta); err != nil {
			log.Warn("update event push failed",
				zap.String("sandbox_id", ev.SandboxID), zap.Error(err))
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
