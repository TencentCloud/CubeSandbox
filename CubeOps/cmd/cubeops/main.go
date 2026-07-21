// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"log/slog"
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
		// Config load failed before logging is initialised; use a
		// stdout-only slog so the error is still visible.
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		})))
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	logging.Init(logging.Options{
		Level:      cfg.LogLevel,
		LogDir:     cfg.LogDir,
		FileNum:    cfg.LogFileNum,
		FileSizeMB: cfg.LogFileSize,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialise database + migrations + master key
	s, err := store.New(ctx, cfg.DaoConfig())
	if err != nil {
		slog.Error("failed to initialise database", "error", err)
		os.Exit(1)
	}
	defer s.Close()

	// Bootstrap JWT secret: use JWT_SECRET env var if set, otherwise
	// auto-generate and persist to DB (zero-config deployment).
	jwtSecret, err := s.BootstrapJWTSecret(ctx, cfg.JWTSecret)
	if err != nil {
		slog.Error("failed to bootstrap JWT secret", "error", err)
		os.Exit(1)
	}
	cfg.JWTSecret = jwtSecret

	srv := server.New(cfg, s)

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		slog.Info("received shutdown signal")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("server shutdown error", "error", err)
		}
		cancel()
	}()

	if err := srv.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}

	slog.Info("CubeOps stopped")
}
