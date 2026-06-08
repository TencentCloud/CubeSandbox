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

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/dao"
	_ "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/dao/driver/mysql" // register mysql driver
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
	"github.com/tencentcloud/CubeSandbox/cubelog"
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

	if cfg.OssDBConfig == nil || cfg.InstanceDBConfig == nil {
		CubeLog.WithContext(ctx).Warnf("run in degraded mode: oss/instance db config missing, skip localcache/instancecache/scheduler/sandbox init")
		return nil
	}

	// Run schema migrations BEFORE any business package Init so they all
	// see the HEAD schema. Migration uses the same connection pool the
	// dao facade hands back via dao.Default(); business packages that
	// still use db.Init() get their own pool but talk to the same MySQL
	// instance, so they observe the post-migration schema.
	if err := initDatabaseSchema(ctx, cfg); err != nil {
		return fmt.Errorf("dao migrate: %w", err)
	}

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
	// The schema produced by pkg/base/dao/migrate/migrations is a single
	// catalog covering both the OSS-side tables (t_cube_host_*, t_cube_node_*,
	// ...) and the instance-side tables (t_cube_template_*, t_cube_instance_*,
	// t_cube_sandbox_spec, ...). Running migrations against only one of the
	// two configured databases would silently leave the other half empty, so
	// any deployment that genuinely points the two configs at different
	// physical databases is unsupported and must fail fast at startup.
	if inst, oss := cfg.InstanceDBConfig, cfg.OssDBConfig; inst != nil && oss != nil {
		if inst.Driver != oss.Driver || inst.Addr != oss.Addr || inst.DBName != oss.DBName {
			return fmt.Errorf(
				"dao: instance_db_config and ossdb_config must point to the same physical database "+
					"(instance=%s/%s/%s, oss=%s/%s/%s); split-database deployments are not supported by the current schema",
				inst.Driver, inst.Addr, inst.DBName,
				oss.Driver, oss.Addr, oss.DBName,
			)
		}
	}
	src := cfg.InstanceDBConfig
	if src == nil {
		src = cfg.OssDBConfig
	}
	if src == nil {
		return fmt.Errorf("dao: neither instance_db_config nor ossdb_config is set")
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
