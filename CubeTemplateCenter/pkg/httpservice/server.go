// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package httpservice exposes the template center HTTP API.
//
// By default TC is a build worker: /health, /metrics and the internal
// build-submission endpoint only; the public template API stays on CubeMaster
// (design §1.1 "master 内部闭环"). Setting
// CUBE_TEMPLATE_CENTER_SERVE_TEMPLATE_API=true additionally mounts the public
// template control plane here -- see registerRoutes and pkg/tcconfig.
package httpservice

import (
	"context"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/recov"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/nodemeta"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/cube"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/middleware"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/api"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/tcconfig"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

// Server wraps the internal HTTP server (gin engine + timeouts).
type Server struct {
	InternalHttpServer *internalHttp
}

// New constructs the template center HTTP server.
func New(ctx context.Context, cfg *config.Config) (*Server, error) {
	if cfg == nil || cfg.Common == nil {
		return nil, errors.New("config is nil")
	}
	s := &Server{}
	var err error
	s.InternalHttpServer, err = NewInternalHttp(ctx, cfg)
	if err != nil {
		return nil, err
	}

	config.AppendConfigWatcher(s)
	return s, nil
}

type internalHttp struct {
	*http.Server
	engine *gin.Engine
}

// newEngine builds the gin engine. Identical to CubeMaster/pkg/server/server.go
// — kept as a copy so TC does not need to import CubeMaster's server package
// (which would register CubeMaster-only routes).
func newEngine() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.RedirectTrailingSlash = false
	engine.HandleMethodNotAllowed = true
	engine.NoRoute(func(c *gin.Context) {
		http.NotFound(c.Writer, c.Request)
	})
	engine.NoMethod(func(c *gin.Context) {
		c.AbortWithStatus(http.StatusMethodNotAllowed)
	})
	return engine
}

// NewInternalHttp constructs the internal HTTP server with timeouts from cfg.
func NewInternalHttp(ctx context.Context, cfg *config.Config) (*internalHttp, error) {
	if cfg == nil || cfg.Common == nil {
		return nil, errors.New("config is nil")
	}

	engine := newEngine()
	s := &internalHttp{
		Server: &http.Server{
			Addr:         net.JoinHostPort(cfg.Common.HttpBind, strconv.Itoa(cfg.Common.HttpPort)),
			ReadTimeout:  time.Second * time.Duration(cfg.Common.ReadTimeout),
			WriteTimeout: time.Second * time.Duration(cfg.Common.WriteTimeout),
			IdleTimeout:  time.Second * time.Duration(cfg.Common.IdleTimeout),
			Handler:      engine,
		},
		engine: engine,
	}

	s.registerRoutes()
	return s, nil
}

// registerRoutes mounts the middleware and the TC-owned routes.
//
// Always mounted:
//   - GET  /health              probe
//   - GET  /metrics             prometheus
//   - POST /tc/api/v1/build     build submission from CubeMaster
//
// Mounted only when CUBE_TEMPLATE_CENTER_SERVE_TEMPLATE_API=true: the public
// template control-plane routes (/cube/template*). Default OFF, because in the
// current iteration CubeMaster owns the template API and TC just builds what it
// is told. Serving them while CubeMaster still owns the flow would create a
// shadow entry point -- two processes writing the same tables concurrently.
//
// Switch it on to point cubemastercli straight at TC, or to preview the next
// iteration. See pkg/tcconfig.
//
// The artifact download endpoint stays on CubeMaster in both cases: Cubelet
// pulls ext4 files from CubeMaster, which shares TC's artifact directory (one
// CBS disk / PVC). See docs/dev/templatecenter-design.md §9.7.
func (s *internalHttp) registerRoutes() {
	root := s.engine.Group("")
	root.Use(middleware.GinRequestMiddleware())
	root.GET("/metrics", gin.WrapH(promhttp.Handler()))
	root.GET("/health", s.healthHandler)

	// Internal build-submission API: CubeMaster pushes jobs to
	// POST /tc/api/v1/build; TC builds and reports status back.
	api.RegisterInternalRoutes(root)

	if tcconfig.ServePublicTemplateAPI() {
		CubeLog.WithContext(context.Background()).Errorf(
			"%s is on: serving the public template API from templatecenter",
			tcconfig.EnvServeTemplateAPI)
		cube.RegisterTemplateRoutes(root.Group(cube.CubeURI()))
	}
}

// healthHandler implements the readiness probe.
//
// Baseline readiness is a single condition: the store is attached, hence the DB
// is reachable and the reconciler can take its session lock.
//
// The node view is checked ONLY when TC serves the public template API,
// because that is the only configuration in which TC picks distribution
// targets itself. In the default configuration CubeMaster owns node health and
// TC never reads it, so requiring it would make TC report not-ready over
// something it does not use.
func (s *internalHttp) healthHandler(c *gin.Context) {
	storeReady := templatecenter.IsReady()
	checks := gin.H{"templatecenter_store": storeReady}
	ready := storeReady

	if tcconfig.ServePublicTemplateAPI() {
		nodeReady := nodemeta.Ready()
		checks["nodemeta"] = nodeReady
		ready = ready && nodeReady
	}

	if ready {
		c.JSON(http.StatusOK, gin.H{
			"status": "ok",
			"checks": checks,
		})
		return
	}

	c.JSON(http.StatusServiceUnavailable, gin.H{
		"status": "not_ready",
		"checks": checks,
	})
}

func (s *internalHttp) Start() error {
	if err := s.ListenAndServe(); err != nil {
		if err == http.ErrServerClosed {
			return nil
		}
		return errors.WithStack(err)
	}
	return nil
}

// Run starts the HTTP server in a goroutine.
func (s *Server) Run() {
	if s.InternalHttpServer != nil {
		go func() {
			if err := s.InternalHttpServer.Start(); err != nil {
				CubeLog.Errorf("templatecenter ListenAndServe:%v", err)
			}
		}()
	}
}

// OnEvent handles config change notifications (log level updates).
func (s *Server) OnEvent(cfg *config.Config) {
	log.OnChangeConf(cfg.Log)
}

// Stop gracefully shuts down the HTTP server.
func (s *Server) Stop() {
	ppid := os.Getpid()
	CubeLog.Errorf("templatecenter stopped gracefully begin, pid %v", ppid)
	wg := sync.WaitGroup{}
	recov.GoWithWaitGroup(&wg, func() {
		if s.InternalHttpServer != nil {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := s.InternalHttpServer.Shutdown(ctx); err != nil {
				CubeLog.Fatal("templatecenter InternalHttp Shutdown:", err)
			}
			select {
			case <-ctx.Done():
				CubeLog.Error("templatecenter InternalHttp Shutdown timeout")
			default:
				CubeLog.Error("templatecenter InternalHttp Shutdown succ")
			}
		}
	})
	wg.Wait()
}
