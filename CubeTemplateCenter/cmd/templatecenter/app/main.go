// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package app provides the main entry point for the template center service.
package app

import (
	"context"
	"fmt"
	stdlog "log"
	"os"
	"os/signal"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeDB/dao"
	_ "github.com/tencentcloud/CubeSandbox/CubeDB/dao/driver/mysql"    // register mysql driver
	_ "github.com/tencentcloud/CubeSandbox/CubeDB/dao/driver/postgres" // register postgres driver
	"github.com/tencentcloud/CubeSandbox/CubeDB/migrate"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/recov"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/nodemeta"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/api"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/build"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/httpservice"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/reconcile"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/tcconfig"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

// App is the template center application.
type App struct{}

// New constructs a new App.
func New() *App {
	return &App{}
}

// Run boots the template center process.
func (a *App) Run() {
	var (
		start   = time.Now()
		signals = make(chan os.Signal, 2048)
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := config.GetConfig()

	if err := coreInit(ctx, cfg); err != nil {
		stdlog.Fatalf("core init fail:%v", recov.DumpStacktrace(3, err))
		return
	}

	// Logged here rather than where they are detected: the earliest notices are
	// produced by tcconfig.ApplySharedEnvAliases, which has to run before the
	// config file is even located, hence before log.Init.
	for _, notice := range tcconfig.Warnings() {
		CubeLog.WithContext(ctx).Warnf("templatecenter config: %s", notice)
	}
	applyNodeIdentity(ctx, cfg)

	// Build executor: owns every in-flight build so they can be cancelled and
	// drained on shutdown (instead of being hard-killed mid-mkfs and left
	// RUNNING for the reconciler's two-hour timeout), deduped, and bounded.
	// Install it before the server starts serving the build endpoint.
	executor := build.NewExecutor(tcconfig.MaxConcurrentBuilds())
	api.SetBuildExecutor(executor)

	// Artifact deleter: owns S3/local file removal when a template is deleted.
	// CubeMaster only marks the artifact row CLEANUP_PENDING and calls TC's
	// delete endpoint; TC does the actual data removal. The reconciler sweeps
	// Artifact deleter: owns S3/local file removal when a template is deleted.
	// CubeMaster only marks the artifact row CLEANUP_PENDING and calls TC's
	// delete endpoint; TC does the actual data removal. The reconciler sweeps
	// CLEANUP_PENDING rows as a backstop. Shared between the delete endpoint
	// and the reconciler so both paths use the same S3 client and DB handle.
	// The S3 client comes from build.SharedS3Client (lazy singleton), so the
	// deleter and the build path never construct duplicate clients.
	var deleter *build.ArtifactDeleter
	if db := templatecenter.GetDB(); db != nil {
		s3Client, _ := build.SharedS3Client()
		deleter = build.NewArtifactDeleter(db, s3Client)
		api.SetArtifactDeleter(deleter)
	} else {
		CubeLog.WithContext(ctx).Warnf("templatecenter artifact deleter not started: db handle unavailable")
	}

	srv, err := httpservice.New(ctx, cfg)
	if err != nil {
		CubeLog.WithContext(ctx).Errorf("templatecenter http server init:%v", err)
		return
	}

	done := handleSignals(ctx, signals, cancel)
	signal.Notify(signals, handledSignals...)

	recov.GoWithRecover(func() {
		srv.Run()
	})

	// Background sweep that fails jobs abandoned mid-build (design §7.3) and
	// backstop-deletes CLEANUP_PENDING artifacts. Cross-replica mutual
	// exclusion is handled inside via the DB session lock, so every replica
	// can start it unconditionally.
	if reconcile.Disabled() {
		CubeLog.WithContext(ctx).Warnf("templatecenter reconciler disabled by %s",
			tcconfig.EnvReconcileDisabled)
	} else if db := templatecenter.GetDB(); db != nil {
		reconciler := reconcile.New(db)
		if deleter != nil {
			reconciler.SetDeleter(deleter)
		}
		recov.GoWithRecover(func() {
			reconciler.Run(ctx)
		})
	} else {
		CubeLog.WithContext(ctx).Errorf("templatecenter reconciler not started: db handle unavailable")
	}

	CubeLog.WithContext(ctx).Errorf("templatecenter successfully booted in %fs", time.Since(start).Seconds())
	<-done

	// Graceful shutdown. Cancel in-flight builds first so they unwind and
	// best-effort report their terminal state, then drain HTTP. The order
	// matters: stopping the server first would let a still-running build keep
	// reporting after the process is half-shut-down.
	CubeLog.WithContext(ctx).Errorf("templatecenter shutting down: cancelling in-flight builds")
	executor.Shutdown()
	srv.Stop()
}

// applyNodeIdentity records this instance's node address and narrows the HTTP
// listen address when it is safe to do so.
//
// MUST run before httpservice.New, which freezes the listen address into
// http.Server.Addr.
//
// The node IP is primarily an identity: TC is a single instance sharing a
// database with CubeMaster, so a log line that does not say which host produced
// it is hard to correlate against CubeMaster's own logs during a build failure.
// Narrowing the bind is secondary and conditional — see
// tcconfig.ResolveHTTPBind, which refuses to bind to an address this host does
// not own so the same manifest works in a Pod and under systemd.
func applyNodeIdentity(ctx context.Context, cfg *config.Config) {
	nodeIP := tcconfig.NodeIP()
	if nodeIP == "" {
		return
	}
	CubeLog.WithContext(ctx).Errorf("templatecenter node identity: %s=%s", tcconfig.EnvNodeIP, nodeIP)

	if cfg == nil || cfg.Common == nil {
		return
	}
	bind, note := tcconfig.ResolveHTTPBind(cfg.Common.HttpBind)
	if note != "" {
		CubeLog.WithContext(ctx).Warnf("templatecenter listen address: %s", note)
	}
	cfg.Common.HttpBind = bind
}

// coreInit wires the minimum set of dependencies the template center needs.
// It intentionally skips CubeMaster-only services (scheduler/sandbox/lifecycle)
// because template center does not own sandbox creation.
func coreInit(ctx context.Context, cfg *config.Config) error {
	log.Init(config.GetLogConfig())

	errorcode.InitCubeCodeRetryMap(cfg)

	if cfg.InstanceDBConfig == nil {
		return fmt.Errorf("instance_db_config is required for template center")
	}

	// Schema migration (same GET_LOCK-protected path as CubeMaster).
	if err := initDatabaseSchema(ctx, cfg); err != nil {
		return fmt.Errorf("dao migrate: %w", err)
	}

	// The node view is always loaded: TC serves /cube/template* directly (see
	// pkg/httpservice.registerRoutes), and its distribution step has to pick
	// target nodes -- distributeRootfsArtifact -> resolveTemplateNodes ->
	// healthyTemplateNodes -> localcache.GetHealthyNodesByInstanceType, whose
	// data only exists because nodemeta.Init registers the node loader
	// (localcache.RegisterNodeLoader). Without it every build would fail with
	// ErrNoTemplateNodes.
	//
	// Node heartbeats only ever reach CubeMaster, so this view is a DB-derived
	// replica, not an authoritative one.
	if err := nodemeta.Init(ctx); err != nil {
		return fmt.Errorf("nodemeta init: %w", err)
	}

	// localcache is always required: the pull-progress live snapshots
	// (pkg/build/progress.go) go through it into Redis, using the same keys
	// CubeMaster reads when answering a progress query.
	if err := localcache.Init(ctx); err != nil {
		return fmt.Errorf("localcache init: %w", err)
	}

	// Attach the templatecenter store. TC uses it for the DB session locks
	// taken by the background reconciler; all business-state writes go through
	// CubeMaster's status callback, not from here.
	if err := templatecenter.InitForTemplateCenter(ctx); err != nil {
		return fmt.Errorf("templatecenter init: %w", err)
	}

	return nil
}

// initDatabaseSchema opens the canonical dao handle and runs pending
// migrations. Mirrors CubeMaster's behavior so the two processes share
// schema ownership.
func initDatabaseSchema(ctx context.Context, cfg *config.Config) error {
	src := cfg.InstanceDBConfig
	if src == nil {
		return fmt.Errorf("dao: instance_db_config is not set")
	}
	daoCfg := dao.Config{
		Driver:                      src.Driver,
		Addr:                        src.Addr,
		User:                        src.User,
		Pwd:                         src.Pwd,
		DBName:                      src.DBName,
		ConnTimeoutSeconds:          src.ConnTimeout,
		ReadTimeoutSeconds:          src.ReadTimeout,
		WriteTimeoutSeconds:         src.WriteTimeout,
		MaxIdleConns:                src.MaxIdleConns,
		MaxOpenConns:                src.MaxOpenConns,
		MaxConnLifeTimeSeconds:      src.MaxConnLifeTimeSeconds,
		MigrationLockTimeoutSeconds: src.MigrationLockTimeoutSeconds,
	}
	if _, err := dao.Open(ctx, daoCfg); err != nil {
		return fmt.Errorf("dao open: %w", err)
	}
	if !migrate.AutoMigrationEnabled() {
		CubeLog.WithContext(ctx).Warnf(
			"CUBE_AUTO_MIGRATION=false: skipping schema migration; DDL must be " +
				"applied out-of-band by a privileged account")
		return nil
	}
	if err := dao.Migrate(ctx); err != nil {
		return fmt.Errorf("dao migrate: %w", err)
	}
	CubeLog.WithContext(ctx).Infof("dao schema migration completed (driver=%s db=%s)",
		daoCfg.Driver, daoCfg.DBName)
	return nil
}
