// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeDB/tombstone"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/config"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/logging"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/server"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/warehouse"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		// Logging is not yet initialised here; write directly to stderr
		// so a config-load failure is always visible regardless of
		// cubelog's default-writer behaviour.
		fmt.Fprintf(os.Stderr, "cubeops: failed to load config: %v\n", err)
		os.Exit(1)
	}

	logging.Init(logging.Options{
		Level:      cfg.LogLevel,
		LogDir:     cfg.LogDir,
		Module:     "cubeops",
		FileNum:    cfg.LogFileNum,
		FileSizeMB: cfg.LogFileSize,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialise database + migrations + master key
	s, err := store.New(ctx, cfg.DaoConfig())
	if err != nil {
		logging.G(ctx).Errorf("failed to initialise database: err=%q", err.Error())
		os.Exit(1)
	}
	defer s.Close()

	// Launch the scheduled tombstone purger (issue #973): hard-purges
	// soft-deleted agenthub rows older than the configured retention.
	// Stops when ctx is cancelled (SIGINT/SIGTERM). DISABLED by default: the
	// purge is irreversible — opt in via soft_delete_purge.enable: true.
	sp := cfg.SoftDeletePurge
	spEnabled := false
	if sp.Enable != nil {
		spEnabled = *sp.Enable
	}
	tombstone.Start(ctx, s.DB(), tombstone.Config{
		Enabled:   spEnabled,
		DryRun:    sp.DryRun,
		Retention: sp.Retention,
		Interval:  sp.Interval,
		Tables:    []string{store.AgenthubInstanceTable, store.AgenthubSnapshotTable, store.AgenthubTemplateTable},
		LockName:  "cubeops_tombstone_purge_v1",
	})

	// Bootstrap JWT secret: use JWT_SECRET env var if set, otherwise
	// auto-generate and persist to DB (zero-config deployment).
	jwtSecret, err := s.BootstrapJWTSecret(ctx, cfg.JWTSecret)
	if err != nil {
		logging.G(ctx).Errorf("failed to bootstrap JWT secret: err=%q", err.Error())
		os.Exit(1)
	}
	cfg.JWTSecret = jwtSecret

	srv := server.New(cfg, s, initWarehouseBlobs(ctx, cfg))

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		logging.G(context.Background()).Info("received shutdown signal")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logging.G(shutdownCtx).Errorf("server shutdown error: err=%q", err.Error())
		}
		cancel()
	}()

	if err := srv.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logging.G(ctx).Errorf("server error: err=%q", err.Error())
		os.Exit(1)
	}

	logging.G(ctx).Info("CubeOps stopped")
}

func initWarehouseBlobs(ctx context.Context, cfg *config.Config) warehouse.BlobStore {
	if !cfg.S3Configured() {
		slog.Warn("component warehouse disabled: S3 is not configured")
		return nil
	}
	blobs, err := warehouse.NewS3BlobStore(cfg.S3, cfg.Warehouse.UploadTimeout)
	if err != nil {
		slog.Warn("component warehouse disabled: s3 client", "error", err)
		return nil
	}
	if err := probeWarehouseBucket(ctx, blobs); err != nil {
		return blobs
	}
	lctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if err := blobs.EnsureLifecycle(lctx); err != nil {
		slog.Warn("warehouse bucket lifecycle", "error", err)
	}
	return blobs
}

func probeWarehouseBucket(ctx context.Context, blobs warehouse.BlobStore) error {
	delay := time.Second
	var last error
	for i := 0; i < 8; i++ {
		pctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		last = blobs.EnsureBucket(pctx)
		cancel()
		if last == nil {
			return nil
		}
		slog.Warn("warehouse s3 probe failed", "attempt", i+1, "error", last)
		select {
		case <-ctx.Done():
			slog.Warn("warehouse s3 probe canceled; continuing with client")
			return ctx.Err()
		case <-time.After(delay):
		}
		if delay < 15*time.Second {
			delay *= 2
		}
	}
	slog.Warn("warehouse s3 still unreachable; continuing with client", "error", last)
	return last
}
