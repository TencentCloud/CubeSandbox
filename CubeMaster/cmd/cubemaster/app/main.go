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
	"math/rand"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/recov"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet/grpcconn"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/instancecache"
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

	rand.Seed(time.Now().UnixNano())

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

	scheduler.InitScheduler(ctx)

	if err := sandbox.Init(ctx, cfg); err != nil {
		stdlog.Fatalf("cube init fail:%v", err)
		return err
	}

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
