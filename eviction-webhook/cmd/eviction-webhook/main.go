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

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// ── Kubernetes in-cluster client ──────────────────────────────────────
	log.Printf("loading in-cluster config...")
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("in-cluster config: %w", err)
	}
	log.Printf("in-cluster config loaded: host=%s", restCfg.Host)
	k8sClient, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("k8s client: %w", err)
	}
	log.Printf("k8s client created")

	// ── Pod informer — provides label lookups on the hot webhook path ─────
	// Start async to avoid blocking startup. The informer syncs in the background.
	// If sync fails, the handler falls back to UserInfo username for node name.
	log.Printf("starting pod informer (namespace=%q)...", cfg.PodNamespace)
	podCache, err := podinformer.NewAsync(ctx, k8sClient, cfg.PodNamespace)
	if err != nil {
		return fmt.Errorf("pod informer async start: %w", err)
	}
	log.Printf("pod informer started (async sync)")

	// ── Local NDJSON audit store ──────────────────────────────────────────
	auditStore, err := store.New(cfg.AuditLogPath)
	if err != nil {
		return fmt.Errorf("audit store: %w", err)
	}
	defer auditStore.Close()
	log.Printf("audit store opened: %s", cfg.AuditLogPath)

	// ── CubeMaster clients ────────────────────────────────────────────────
	var rep admission.EventReporter
	if cfg.EventReportEnabled {
		// Optional event notification. The recovery path below uses existing
		// CubeMaster APIs and does not require CubeMaster to expose /event/eviction.
		rep = reporter.New(cfg.CubeMasterURL, cfg.AuthUserID, cfg.AuthSecretKey, cfg.AuthEnabled)
		log.Printf("event reporter enabled: target=%s/event/eviction", cfg.CubeMasterURL)
	} else {
		log.Printf("event reporter disabled")
	}

	// cmClient: directly calls CubeMaster isolation + pause/resume APIs
	cmClient := cubemaster.New(cfg.CubeMasterURL, cfg.AuthUserID, cfg.AuthSecretKey, cfg.AuthEnabled)

	// ── Recovery manager ─────────────────────────────────────────────────
	// Tracks which nodes are cordoned and which sandboxes are paused, and
	// drives CubeMaster to cordon/uncordon + pause/resume.
	// State is persisted to disk so the webhook can recover after restart.
	statePath := envOrDefault("RECOVERY_STATE_PATH", "/var/lib/eviction-webhook/recovery-state.json")
	recoveryMgr, err := recovery.NewWithPersister(cmClient, statePath)
	if err != nil {
		log.Printf("recovery manager: %v (continuing without persisted state)", err)
		recoveryMgr = recovery.New(cmClient)
	}
	nodePressure := func(ctx context.Context, nodeName string) (bool, bool, error) {
		node, err := k8sClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false, false, err
		}
		return nodewatch.HasMemoryPressure(node), nodewatch.HasResourcePressure(node), nil
	}
	recoveryMgr.SetPressureChecker(func(ctx context.Context, nodeName string) (bool, error) {
		_, underResourcePressure, err := nodePressure(ctx, nodeName)
		return underResourcePressure, err
	})

	// ── Node watcher — triggers recovery on MemoryPressure transitions ───
	// onPressureDetected handles the case where kubelet's internal eviction
	// bypasses the API server (and thus the webhook never sees an Eviction).
	log.Printf("starting node watcher...")
	if err := nodewatch.StartAsyncWithPressureDetected(ctx, k8sClient, recoveryMgr.OnPressureRelief, recoveryMgr.OnPressureDetected); err != nil {
		log.Printf("node watcher: %v (continuing anyway)", err)
	}
	log.Printf("node watcher started (async)")
	recoveryMgr.ReconcileRestored(ctx)

	// ── Admission handler ─────────────────────────────────────────────────
	handler := admission.NewWithRecovery(podCache, auditStore, rep, recoveryMgr)
	handler.SetPressureChecker(func(ctx context.Context, nodeName string) (bool, error) {
		underMemoryPressure, _, err := nodePressure(ctx, nodeName)
		return underMemoryPressure, err
	})

	// ── HTTP mux ──────────────────────────────────────────────────────────
	mux := http.NewServeMux()
	mux.Handle("/webhook/eviction", handler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// ── TLS server ────────────────────────────────────────────────────────
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
		log.Printf("listening on %s (TLS)", cfg.ListenAddr)
		if err := srv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			serverErr <- fmt.Errorf("serve: %w", err)
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-serverErr:
		return err
	}

	log.Println("shutting down")
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	return nil
}

type config struct {
	ListenAddr         string
	TLSCertFile        string
	TLSKeyFile         string
	PodNamespace       string
	AuditLogPath       string
	CubeMasterURL      string
	EventReportEnabled bool
	AuthEnabled        bool
	AuthUserID         string
	AuthSecretKey      string
}

func loadConfig() (config, error) {
	cubeMasterURL := os.Getenv("CUBE_MASTER_URL")
	if cubeMasterURL == "" {
		return config{}, fmt.Errorf("required environment variable CUBE_MASTER_URL is not set")
	}
	return config{
		ListenAddr:         envOrDefault("LISTEN_ADDR", ":8443"),
		TLSCertFile:        envOrDefault("TLS_CERT_FILE", "/etc/eviction-webhook/tls/tls.crt"),
		TLSKeyFile:         envOrDefault("TLS_KEY_FILE", "/etc/eviction-webhook/tls/tls.key"),
		PodNamespace:       os.Getenv("POD_NAMESPACE"),
		AuditLogPath:       envOrDefault("AUDIT_LOG_PATH", "/var/log/eviction-webhook/events.ndjson"),
		CubeMasterURL:      cubeMasterURL,
		EventReportEnabled: os.Getenv("EVENT_REPORT_ENABLE") == "true",
		AuthEnabled:        os.Getenv("CUBE_AUTH_ENABLE") == "true",
		AuthUserID:         os.Getenv("CUBE_AUTH_USER_ID"),
		AuthSecretKey:      os.Getenv("CUBE_AUTH_SECRET_KEY"),
	}, nil
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
