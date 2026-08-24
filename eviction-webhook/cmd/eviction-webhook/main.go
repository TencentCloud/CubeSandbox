// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/tencentcloud/CubeSandbox/eviction-webhook/internal/admission"
	"github.com/tencentcloud/CubeSandbox/eviction-webhook/internal/cubemaster"
	"github.com/tencentcloud/CubeSandbox/eviction-webhook/internal/nodewatch"
	"github.com/tencentcloud/CubeSandbox/eviction-webhook/internal/podinformer"
	"github.com/tencentcloud/CubeSandbox/eviction-webhook/internal/recovery"
	"github.com/tencentcloud/CubeSandbox/eviction-webhook/internal/reporter"
	"github.com/tencentcloud/CubeSandbox/eviction-webhook/internal/store"
	"github.com/tencentcloud/CubeSandbox/eviction-webhook/internal/telemetry"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("%v", err)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	// ── Structured logger ─────────────────────────────────────────────────
	logger, err := telemetry.InitLogger(cfg.Debug)
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer logger.Sync() //nolint:errcheck

	logger.Info("eviction-webhook starting",
		zap.String("ListenAddr", cfg.ListenAddr),
		zap.String("MetricsAddr", cfg.MetricsAddr),
		zap.String("CubeMasterURL", cfg.CubeMasterURL),
	)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// ── Metrics server (non-TLS, separate port) ───────────────────────────
	go startMetricsServer(ctx, cfg.MetricsAddr, logger)

	// ── Kubernetes in-cluster client ──────────────────────────────────────
	logger.Info("loading in-cluster config")
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("in-cluster config: %w", err)
	}
	k8sClient, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("k8s client: %w", err)
	}
	logger.Info("k8s client created", zap.String("Host", restCfg.Host))

	// ── Pod informer ──────────────────────────────────────────────────────
	podCache, err := podinformer.NewAsync(ctx, k8sClient, cfg.PodNamespace)
	if err != nil {
		return fmt.Errorf("pod informer async start: %w", err)
	}
	logger.Info("pod informer started", zap.String("Namespace", cfg.PodNamespace))

	// ── Audit store ───────────────────────────────────────────────────────
	auditStore, err := store.New(cfg.AuditLogPath)
	if err != nil {
		return fmt.Errorf("audit store: %w", err)
	}
	defer auditStore.Close()
	logger.Info("audit store opened", zap.String("Path", cfg.AuditLogPath))

	// ── CubeMaster clients ────────────────────────────────────────────────
	var rep admission.EventReporter
	if cfg.EventReportEnabled {
		rep = reporter.New(cfg.CubeMasterURL, cfg.AuthUserID, cfg.AuthSecretKey, cfg.AuthEnabled)
		logger.Info("event reporter enabled", zap.String("Target", cfg.CubeMasterURL))
	} else {
		logger.Info("event reporter disabled")
	}

	cmClient := cubemaster.New(cfg.CubeMasterURL, cfg.AuthUserID, cfg.AuthSecretKey, cfg.AuthEnabled)

	memoryPressure := func(ctx context.Context, nodeName string) (bool, error) {
		node, err := k8sClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return nodewatch.HasMemoryPressure(node), nil
	}

	// ── Recovery manager ──────────────────────────────────────────────────
	// RECOVERY_ENABLE=false still denies the eviction (the MicroVM is protected)
	// but skips the cordon/pause/resume side effect entirely: no node watcher,
	// no persisted recovery state, no CubeMaster isolate/pause/resume calls.
	var handlerRecoveryMgr admission.RecoveryManager
	if cfg.RecoveryEnabled {
		statePath := envOrDefault("RECOVERY_STATE_PATH", "/var/lib/eviction-webhook/recovery-state.json")
		recoveryMgr, err := recovery.NewWithPersister(cmClient, statePath)
		if err != nil {
			return fmt.Errorf("initialize recovery manager: %w", err)
		}
		recoveryMgr.SetPressureChecker(func(ctx context.Context, nodeName string) (bool, error) {
			return memoryPressure(ctx, nodeName)
		})

		logger.Info("starting node watcher")
		if err := nodewatch.StartAsyncWithPressureDetected(ctx, k8sClient, recoveryMgr.OnPressureRelief, recoveryMgr.OnPressureDetected); err != nil {
			logger.Warn("node watcher failed to start", zap.Error(err))
		}
		recoveryMgr.ReconcileRestored(ctx)
		handlerRecoveryMgr = recoveryMgr
	} else {
		logger.Info("recovery disabled (RECOVERY_ENABLE=false): evictions will still be denied, but no cordon/pause/resume will be triggered")
	}

	// ── Admission handler ─────────────────────────────────────────────────
	handler := admission.NewWithLogger(podCache, auditStore, rep, handlerRecoveryMgr, logger)
	handler.SetPressureChecker(func(ctx context.Context, nodeName string) (bool, error) {
		return memoryPressure(ctx, nodeName)
	})

	// ── Webhook TLS server ────────────────────────────────────────────────
	mux := http.NewServeMux()
	mux.Handle("/webhook/eviction", handler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	tlsCert, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
	if err != nil {
		return fmt.Errorf("load TLS cert: %w", err)
	}
	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: mux,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{tlsCert},
			MinVersion:   tls.VersionTLS12,
		},
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("webhook server listening (TLS)", zap.String("Addr", cfg.ListenAddr))
		if err := srv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			serverErr <- fmt.Errorf("serve: %w", err)
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-serverErr:
		return err
	}

	logger.Info("shutting down")
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		logger.Warn("shutdown error", zap.Error(err))
	}
	return nil
}

func startMetricsServer(ctx context.Context, addr string, logger *zap.Logger) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok")) //nolint:errcheck
	})

	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}

	logger.Info("metrics server starting", zap.String("Addr", addr))
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("metrics server error", zap.Error(err))
		}
	}()
	<-ctx.Done()
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shutCtx) //nolint:errcheck
}

type config struct {
	ListenAddr         string
	MetricsAddr        string
	TLSCertFile        string
	TLSKeyFile         string
	PodNamespace       string
	AuditLogPath       string
	CubeMasterURL      string
	EventReportEnabled bool
	RecoveryEnabled    bool
	AuthEnabled        bool
	AuthUserID         string
	AuthSecretKey      string
	Debug              bool
}

func loadConfig() (config, error) {
	cubeMasterURL := os.Getenv("CUBE_MASTER_URL")
	if cubeMasterURL == "" {
		return config{}, fmt.Errorf("required environment variable CUBE_MASTER_URL is not set")
	}
	authEnabled := os.Getenv("CUBE_AUTH_ENABLE") == "true"
	authUserID := os.Getenv("CUBE_AUTH_USER_ID")
	authSecretKey := os.Getenv("CUBE_AUTH_SECRET_KEY")
	if authEnabled && (authUserID == "" || authSecretKey == "") {
		return config{}, fmt.Errorf("CUBE_AUTH_ENABLE=true requires CUBE_AUTH_USER_ID and CUBE_AUTH_SECRET_KEY")
	}
	return config{
		ListenAddr:         envOrDefault("LISTEN_ADDR", ":8443"),
		MetricsAddr:        envOrDefault("METRICS_ADDR", ":8888"),
		TLSCertFile:        envOrDefault("TLS_CERT_FILE", "/etc/eviction-webhook/tls/tls.crt"),
		TLSKeyFile:         envOrDefault("TLS_KEY_FILE", "/etc/eviction-webhook/tls/tls.key"),
		PodNamespace:       os.Getenv("POD_NAMESPACE"),
		AuditLogPath:       envOrDefault("AUDIT_LOG_PATH", "/var/log/eviction-webhook/events.ndjson"),
		CubeMasterURL:      cubeMasterURL,
		EventReportEnabled: os.Getenv("EVENT_REPORT_ENABLE") == "true",
		RecoveryEnabled:    os.Getenv("RECOVERY_ENABLE") != "false",
		AuthEnabled:        authEnabled,
		AuthUserID:         authUserID,
		AuthSecretKey:      authSecretKey,
		Debug:              os.Getenv("DEBUG") == "true",
	}, nil
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
