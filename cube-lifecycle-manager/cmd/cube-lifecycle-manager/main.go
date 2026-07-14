// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// cube-lifecycle-manager drives the auto-pause / auto-resume loop that sits
// between CubeMaster, CubeProxy, and Redis. It supersedes the older
// in-container "cube-proxy-sidecar"; the wire protocol with CubeProxy
// (admin push endpoints + /_sidecar_resume callback) is unchanged.
package main

import (
	"context"
	"errors"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/config"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/cubemasterclient"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/discovery"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/httpapi"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/lifecycle"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/proxypush"
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
		zap.String("redis_addr", cfg.RedisAddr),
		zap.Strings("cube_proxy_admin_urls", cfg.CubeProxyAdminURLs),
		zap.String("cubemaster_url", cfg.CubeMasterURL),
		zap.String("listen_addr", cfg.ListenAddr),
		zap.String("consumer_group", cfg.ConsumerGroup),
		zap.String("consumer_name", cfg.ConsumerName))

	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
		DB:       cfg.RedisDB,
	})
	defer func() { _ = rdb.Close() }()

	stream := redisstream.New(rdb, logger.Named("redis"))
	masterClient := cubemasterclient.New(cfg.CubeMasterURL, cfg.HTTPTimeout)
	reg := registry.New()

	rootCtx, cancel := signalContext()
	defer cancel()

	// startupTs marks the boundary between "bootstrap entries (HGETALL)"
	// and "stream entries (XREADGROUP)" for the sweeper's warmup logic.
	startupTs := time.Now()

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

	// 1. Bootstrap the in-memory registry from the meta HSet. We do NOT push
	//    entries to CubeProxy from here — the onJoin callback (or the static
	//    fleet's initial replay below) is the single point that hydrates each
	//    proxy. This keeps the "who pushes what to whom" invariant simple:
	//    every meta hits every proxy exactly through the onJoin replay + the
	//    stream consumer loop.
	if err := bootstrapRegistry(rootCtx, stream, reg, startupTs, logger); err != nil {
		return err
	}
	if staticFleet != nil {
		// Static fleet doesn't emit onJoin events, so replay explicitly.
		for _, ep := range staticFleet.Snapshot() {
			replayRegistryTo(rootCtx, pushClient, reg, ep, logger.Named("replay"))
		}
	}

	// 2. Ensure the consumer group exists.
	if err := stream.EnsureGroup(rootCtx, cfg.ConsumerGroup); err != nil {
		return err
	}

	resumeImpl := resumer.New(resumer.Options{
		Registry:     reg,
		Redis:        stream,
		CubeMaster:   masterClient,
		ProxyPush:    pushClient,
		StateLockTTL: cfg.StateLockTTL,
		Log:          logger.Named("resumer"),
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
	})

	apiSrv := httpapi.New(cfg.ListenAddr, resumeImpl, reg, logger.Named("http")).
		WithFleetSizer(fleetSizer{fleet})

	// 3. Run all background loops concurrently. First error cancels the rest.
	loopCount := 4
	if discSvc != nil {
		loopCount++
	}
	stateSyncDeps := statesync.Deps{
		Registry:  reg,
		Redis:     stream,
		ProxyPush: pushClient,
		TTL:       cfg.StateLockTTL,
		Log:       logger.Named("statesync"),
	}

	errs := make(chan error, loopCount)
	go func() {
		errs <- consumeStream(rootCtx, stream, pushClient, reg, cfg, stateSyncDeps, logger.Named("stream"))
	}()
	go func() { errs <- pollLastActive(rootCtx, pushClient, reg, cfg.LastActivePoll, logger.Named("active")) }()
	go func() { errs <- sweep.Run(rootCtx) }()
	go func() { errs <- apiSrv.Run(rootCtx) }()
	if discSvc != nil {
		go func() { errs <- discSvc.Run(rootCtx) }()
	}

	// First loop to return wins; we cancel siblings via context and drain.
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
// endpoint. Used both by discovery.OnJoin (when a new CubeProxy arrives) and
// by the static-fleet initialization path. Errors are logged but not
// escalated: reconciliation eventually converges via the stream consumer.
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
// It does NOT push to CubeProxy: fleet hydration is the discovery.OnJoin
// callback's job (or, for the static-fleet dev path, an explicit replay call
// in run()). Keeping registry seeding and admin pushes separate simplifies
// the invariant "every meta reaches every proxy through onJoin + stream".
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
