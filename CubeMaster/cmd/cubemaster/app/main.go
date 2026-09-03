// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package app provides the main entry point for the application.
package app

import (
	"context"
	"expvar"
	"fmt"
	stdlog "log"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeDB/dao"
	_ "github.com/tencentcloud/CubeSandbox/CubeDB/dao/driver/mysql"    // register mysql driver
	_ "github.com/tencentcloud/CubeSandbox/CubeDB/dao/driver/postgres" // register postgres driver
	"github.com/tencentcloud/CubeSandbox/CubeDB/migrate"
	"github.com/tencentcloud/CubeSandbox/CubeDB/tombstone"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/recov"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet/grpcconn"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/instancecache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/lifecycle"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/nodemeta"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/server"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/task"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	volumeplugin "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/volume/plugin"
	_ "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/volume/plugin/binary"
	_ "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/volume/plugin/rpc"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

type App struct {
}

func New() *App {
	return &App{}
}

func (a *App) Run() {
	var (
		start       = time.Now()
		signals     = make(chan os.Signal, 2048)
		serverC     = make(chan *server.Server, 1)
		ctx, cancel = context.WithCancel(context.Background())
	)
	defer cancel()

	cfg := config.GetConfig()

	if err := coreInit(ctx, cfg); err != nil {
		stdlog.Fatalf("core init fail:%v", recov.DumpStacktrace(3, err))
		return
	}

	type srvResp struct {
		s   *server.Server
		err error
	}

	chsrv := make(chan srvResp)
	go func() {
		defer close(chsrv)

		serverTmp, err := server.New(ctx, cfg)
		if err != nil {
			select {
			case chsrv <- srvResp{err: err}:
			case <-ctx.Done():
			}
			return
		}

		select {
		case <-ctx.Done():
			serverTmp.Stop()
		case chsrv <- srvResp{s: serverTmp}:
		}
	}()

	var serverTmp *server.Server
	select {
	case <-ctx.Done():
		CubeLog.WithContext(ctx).Errorf("cubemaster start:%v", ctx.Err())
		return
	case r := <-chsrv:
		if r.err != nil {
			CubeLog.WithContext(ctx).Errorf("cubemaster start:%v", r.err)
			return
		}
		serverTmp = r.s
	}

	select {
	case <-ctx.Done():
		CubeLog.WithContext(ctx).Errorf("cubemaster start:%v", ctx.Err())
		return
	case serverC <- serverTmp:
	}

	done := handleSignals(ctx, signals, serverC, cancel)

	signal.Notify(signals, handledSignals...)

	recov.GoWithRecover(func() {
		serverTmp.Run()
	})

	serveDebug(ctx, cfg)

	CubeLog.WithContext(ctx).Errorf("cubemaster successfully booted in %fs", time.Since(start).Seconds())
	<-done
}

func coreInit(ctx context.Context, cfg *config.Config) error {

	log.Init(config.GetLogConfig())

	errorcode.InitCubeCodeRetryMap(cfg)

	task.InitTask(ctx, cfg)

	grpcconn.Init(ctx)

	if cfg.InstanceDBConfig == nil {
		CubeLog.WithContext(ctx).Warnf("run in degraded mode: instance db config missing, skip localcache/instancecache/scheduler/sandbox init")
		return nil
	}

	// Run schema migrations BEFORE any business package Init so they all
	// see the HEAD schema. Migration uses the single dao.Default() handle
	// that dao.Open establishes below, so every business package observes
	// the post-migration schema.
	if err := initDatabaseSchema(ctx, cfg); err != nil {
		return fmt.Errorf("dao migrate: %w", err)
	}

	// Launch the scheduled tombstone purger (issue #973): hard-purges
	// soft-deleted rows older than the configured retention. Registered
	// here (not in templatecenter.Init) because it is a DB-level concern;
	// it runs only after the schema is migrated and dao.Default() is
	// usable, and stops when ctx is cancelled. DISABLED by default: the purge is
	// irreversible, so operators must opt in via soft_delete_purge.enable: true
	// (review: upgrading must not silently hard-delete retained tombstones).
	sp := cfg.SoftDeletePurge
	spEnabled := false
	if sp != nil && sp.Enable != nil {
		spEnabled = *sp.Enable
	}
	var spRetention, spInterval time.Duration
	if sp != nil {
		spRetention = sp.Retention
		spInterval = sp.Interval
	}
	tombstone.Start(ctx, dao.Default(), tombstone.Config{
		Enabled:   spEnabled,
		DryRun:    sp != nil && sp.DryRun,
		Retention: spRetention,
		Interval:  spInterval,
		TablesFn: func() []string {
			// Resolve per pass from the LIVE config so the purge table list tracks
			// disable_hard_delete across a hot-reload (the app reads it live on every
			// delete); a boot-time snapshot would otherwise diverge and purge rows the
			// operator just chose to retain.
			c := config.GetConfig()
			return cubeMasterPurgeTables(c != nil && c.Common != nil && c.Common.DisableHardDelete)
		},
		LockName: "cubemaster_tombstone_purge_v1",
	})

	if err := nodemeta.Init(ctx); err != nil {
		stdlog.Fatalf("nodemeta init fail:%v", err)
		return err
	}

	if err := localcache.Init(ctx); err != nil {
		stdlog.Fatalf("localcache init fail:%v", err)
		return err
	}

	if err := instancecache.Init(ctx); err != nil {
		stdlog.Fatalf("localcache init fail:%v", err)
		return err
	}

	if err := templatecenter.Init(ctx); err != nil {
		stdlog.Fatalf("templatecenter init fail:%v", err)
		return err
	}

	if err := initVolumePlugins(cfg); err != nil {
		stdlog.Fatalf("volume plugin init fail:%v", err)
		return err
	}

	// lifecycle wires the auto-pause / auto-resume metadata channel into the
	// sandbox create/destroy hooks. It is non-fatal: a Redis hiccup must not
	// block CubeMaster from serving sandboxes, only the sidecar's view goes
	// stale until the next reconcile.
	if err := lifecycle.Init(ctx); err != nil {
		log.G(ctx).Warnf("lifecycle init fail (non-fatal): %v", err)
	}

	scheduler.InitScheduler(ctx)

	if err := sandbox.Init(ctx, cfg); err != nil {
		stdlog.Fatalf("cube init fail:%v", err)
		return err
	}

	return nil
}

// initDatabaseSchema opens the canonical dao handle and runs every
// pending migration. The cluster-wide GET_LOCK held by the driver's
// SessionLocker serialises this across CubeMaster instances starting up
// in parallel, so it is safe to invoke unconditionally from every
// process; whoever loses the lock race blocks until the winner is done,
// then sees the schema is already at HEAD and returns immediately.
func initDatabaseSchema(ctx context.Context, cfg *config.Config) error {
	// Migrations put every table in the one configured database; build the
	// dao config through the shared helper so both dao.Open identities match.
	daoCfg, err := db.ConfigFromDBConfig(cfg.InstanceDBConfig)
	if err != nil {
		return fmt.Errorf("dao: %w", err)
	}
	if _, err := dao.Open(ctx, daoCfg); err != nil {
		return fmt.Errorf("dao open: %w", err)
	}
	// A runtime account with no DDL permission cannot run the migrator (not even
	// the fingerprint CREATE TABLE), so let such a deployment skip migration and
	// apply schema out-of-band with a privileged account. Default is enabled,
	// so unset/typo never disables migration.
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

func graceFullStop() {
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(config.GetConfig().Common.GraceFullStopTimeoutInSec)*time.Second)
	defer cancel()

	done := make(chan int)
	go func() {

		task.Stop(ctx)
		scheduler.Stop(ctx)
		close(done)
	}()

	select {
	case <-ctx.Done():
		CubeLog.WithContext(ctx).Error("graceFullStop timeout")
	case <-done:
		CubeLog.WithContext(ctx).Error("graceFullStop succ")
	}
}

func serveDebug(ctx context.Context, cfg *config.Config) error {
	if cfg.Common.Debug.Address != "" {
		if l, err := net.Listen("tcp", cfg.Common.Debug.Address); err != nil {
			CubeLog.Errorf("cubemaster start debug:%v", fmt.Errorf("failed to get listener for debug endpoint: %w", err))
			return err
		} else {
			recov.GoWithRecover(func() {
				recov.GoWithRecover(func() {
					<-ctx.Done()
					CubeLog.Infof("cubemaster stop debug")
					_ = l.Close()
				})
				m := http.NewServeMux()
				m.Handle("/debug/vars", expvar.Handler())
				m.Handle("/debug/pprof/", http.HandlerFunc(pprof.Index))
				m.Handle("/debug/pprof/cmdline", http.HandlerFunc(pprof.Cmdline))
				m.Handle("/debug/pprof/profile", http.HandlerFunc(pprof.Profile))
				m.Handle("/debug/pprof/symbol", http.HandlerFunc(pprof.Symbol))
				m.Handle("/debug/pprof/trace", http.HandlerFunc(pprof.Trace))
				m.Handle("/debug/loglevel", http.HandlerFunc(setLogLevel))

				if err := trapClosedConnErr(http.Serve(l, m)); err != nil {
					stdlog.Fatalf("serve failure,%v", err)
				}
			})
		}
	}
	return nil
}

func trapClosedConnErr(err error) error {
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func setLogLevel(w http.ResponseWriter, r *http.Request) {
	l := r.FormValue("level")
	if l == "" {
		return
	}
	CubeLog.SetLevel(CubeLog.StringToLevel(strings.ToUpper(l)))
}

// initVolumePlugins registers external Controller Hook Plugins (binary or rpc).
func initVolumePlugins(cfg *config.Config) error {
	if err := volumeplugin.ValidateConfigs(cfg.VolumePlugins); err != nil {
		return err
	}
	for _, pc := range cfg.VolumePlugins {
		switch pc.Type {
		case "binary":
			if err := volumeplugin.LoadBinary(pc); err != nil {
				return fmt.Errorf("load volume plugin %q: %w", pc.Name, err)
			}
			CubeLog.Infof("[volume] registered binary plugin %q at %s", pc.Name, pc.BinaryPath)
		case "rpc":
			if err := volumeplugin.LoadRPC(pc); err != nil {
				return fmt.Errorf("load volume plugin %q: %w", pc.Name, err)
			}
			CubeLog.Infof("[volume] registered rpc plugin %q at %s", pc.Name, pc.SocketPath)
		case "", "builtin":
			// built-in plugins register themselves via init(); nothing to do.
		default:
			return fmt.Errorf("volume plugin %q: unknown type %q", pc.Name, pc.Type)
		}
	}
	return nil
}
