// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/config"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/logging"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/server"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
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

	// Bootstrap JWT secret: use JWT_SECRET env var if set, otherwise
	// auto-generate and persist to DB (zero-config deployment).
	jwtSecret, err := s.BootstrapJWTSecret(ctx, cfg.JWTSecret)
	if err != nil {
		logging.G(ctx).Errorf("failed to bootstrap JWT secret: err=%q", err.Error())
		os.Exit(1)
	}
	cfg.JWTSecret = jwtSecret

	srv := server.New(cfg, s)

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
