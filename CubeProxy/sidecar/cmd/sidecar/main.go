// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// cube-proxy-sidecar drives the auto-pause / auto-resume loop that sits
// between CubeMaster, CubeProxy, and Redis.
package main

import (
	"context"
	"errors"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/tencentcloud/CubeSandbox/CubeProxy/sidecar/internal/config"
	"github.com/tencentcloud/CubeSandbox/CubeProxy/sidecar/internal/cubemasterclient"
	"github.com/tencentcloud/CubeSandbox/CubeProxy/sidecar/internal/httpapi"
	"github.com/tencentcloud/CubeSandbox/CubeProxy/sidecar/internal/lifecycle"
	"github.com/tencentcloud/CubeSandbox/CubeProxy/sidecar/internal/proxypush"
	"github.com/tencentcloud/CubeSandbox/CubeProxy/sidecar/internal/redisstream"
	"github.com/tencentcloud/CubeSandbox/CubeProxy/sidecar/internal/registry"
	"github.com/tencentcloud/CubeSandbox/CubeProxy/sidecar/internal/resumer"
	"github.com/tencentcloud/CubeSandbox/CubeProxy/sidecar/internal/sweeper"
)

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		zap.L().Fatal("sidecar exit", zap.Error(err))
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

	logger.Info("sidecar starting",
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
	pushClient := proxypush.New(cfg.CubeProxyAdminURLs, cfg.CubeAdminToken, cfg.HTTPTimeout, logger.Named("proxypush"))
	masterClient := cubemasterclient.New(cfg.CubeMasterURL, cfg.HTTPTimeout)
	reg := registry.New()

	rootCtx, cancel := signalContext()
	defer cancel()

	// startupTs marks the boundary between "bootstrap entries (HGETALL)"
	// and "stream entries (XREADGROUP)" for the sweeper's warmup logic.
	startupTs := time.Now()

	// 1. Bootstrap registry from HSet, push it all to CubeProxy. After this
	//    the proxy has the full meta map even before any new events arrive.
	if err := bootstrap(rootCtx, stream, pushClient, reg, startupTs, logger); err != nil {
		return err
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

	apiSrv := httpapi.New(cfg.ListenAddr, resumeImpl, reg, logger.Named("http"))

	// 3. Run all background loops concurrently. First error cancels the rest.
	errs := make(chan error, 4)
	go func() { errs <- consumeStream(rootCtx, stream, pushClient, reg, cfg, logger.Named("stream")) }()
	go func() { errs <- pollLastActive(rootCtx, pushClient, reg, cfg.LastActivePoll, logger.Named("active")) }()
	go func() { errs <- sweep.Run(rootCtx) }()
	go func() { errs <- apiSrv.Run(rootCtx) }()

	// First loop to return wins; we cancel siblings via context and drain.
	first := <-errs
	cancel()
	for i := 0; i < 3; i++ {
		<-errs
	}
	return first
}

func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	return ctx, cancel
}

// bootstrap reads the meta HSet, replaces the in-memory registry, and pushes
// every entry to CubeProxy so the proxy starts up with a complete view.
//
// Bootstrap entries get their FirstSeenAt backdated to a fixed startup
// timestamp so the sweeper's BootstrapWarmup gate can distinguish "loaded
// from HGETALL at process start" (FirstSeenAt == startupTs) from "arrived
// later via stream" (FirstSeenAt > startupTs).
func bootstrap(ctx context.Context, stream *redisstream.Client, push *proxypush.Client,
	reg *registry.Registry, startupTs time.Time, log *zap.Logger) error {

	metas, err := stream.Bootstrap(ctx)
	if err != nil {
		return err
	}
	reg.Reset()
	for _, m := range metas {
		reg.Upsert(m)
		// Pin FirstSeenAt to the recorded startup time. Without this,
		// every bootstrap entry would have FirstSeenAt = time.Now() at
		// the moment Upsert ran, which is a moving target and trips the
		// sweeper's "is this a fresh stream event?" check (it compares
		// FirstSeenAt against startedAt with .After() semantics).
		reg.SetFirstSeenAt(m.SandboxID, startupTs)
		if err := push.UpsertMeta(ctx, m); err != nil {
			// Continue: the proxy will receive entries via the stream consumer
			// loop too, so a partial bootstrap isn't fatal. The next periodic
			// reconcile (if/when added) will close the gap.
			log.Warn("bootstrap push failed",
				zap.String("sandbox_id", m.SandboxID), zap.Error(err))
		}
	}
	log.Info("bootstrap complete", zap.Int("entries", len(metas)))
	return nil
}

// consumeStream is the increment-side of the lifecycle channel. It maintains
// the registry + pushes deltas to CubeProxy as create / delete events arrive.
func consumeStream(ctx context.Context, stream *redisstream.Client, push *proxypush.Client,
	reg *registry.Registry, cfg *config.Config, log *zap.Logger) error {

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
			handleEvent(ctx, ev, push, reg, log)
			if err := stream.Ack(ctx, cfg.ConsumerGroup, ev.StreamID); err != nil {
				log.Warn("ack failed",
					zap.String("id", ev.StreamID), zap.Error(err))
			}
		}
	}
}

func handleEvent(ctx context.Context, ev redisstream.Event, push *proxypush.Client,
	reg *registry.Registry, log *zap.Logger) {

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
			zap.Int("timeout_seconds", ev.Meta.TimeoutSeconds),
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
