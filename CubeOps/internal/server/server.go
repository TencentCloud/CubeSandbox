// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/auth"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/config"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/cubemaster"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/handler"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/logging"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement"
	nmhandler "github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/handler"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/nodemetric"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/service"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
	cubelog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

// Server is the CubeOps HTTP server.
type Server struct {
	cfg     *config.Config
	store   *store.Store
	jm      *auth.JWTManager
	httpSrv *http.Server
	cm      *cubemaster.Client
	nodeSvc *nodemanagement.Service
}

// New creates a new CubeOps server.
func New(cfg *config.Config, s *store.Store) *Server {
	jm := auth.NewJWTManager(cfg.JWTSecret, cfg.AccessTTL, cfg.RefreshTTL)
	cm := cubemaster.New(cfg.CubeMasterAddr)
	return &Server{
		cfg:   cfg,
		store: s,
		jm:    jm,
		cm:    cm,
	}
}

// Start begins listening for HTTP requests.
func (s *Server) Start() error {
	if err := nodemetric.Init(s.cfg); err != nil {
		return fmt.Errorf("nodemetric init: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	nodeSvc, err := nodemanagement.New(ctx, s.store.DB(), nodemanagement.DefaultDeclaredVersionInfo())
	if err != nil {
		return fmt.Errorf("init node management service: %w", err)
	}
	// Enable sandbox verification on node deletion via CubeMaster.
	nodeSvc.SetSandboxInventoryChecker(nodemanagement.SandboxInventoryChecker(s.cm))
	s.nodeSvc = nodeSvc

	engine := s.buildRouter()

	s.httpSrv = &http.Server{
		Addr:              s.cfg.Bind,
		Handler:           engine,
		ReadHeaderTimeout: 10 * time.Second,  // mitigate Slowloris attacks
		WriteTimeout:      300 * time.Second, // match nginx proxy_read_timeout
		IdleTimeout:       120 * time.Second,
		// ReadTimeout is intentionally NOT set. Go's http.Server.ReadTimeout
		// covers the entire request body read AND cancels the request context
		// when it expires — which would abort long-running handlers like Agent
		// creation (applyOpenclawRuntime can take 25+ seconds). ReadHeaderTimeout
		// alone is sufficient for Slowloris mitigation.
	}

	logging.G(context.Background()).Infof("CubeOps starting on %s", s.cfg.Bind)
	return s.httpSrv.ListenAndServe()
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpSrv == nil {
		return nil
	}
	logging.G(ctx).Info("CubeOps shutting down")
	return s.httpSrv.Shutdown(ctx)
}

// buildRouter configures all routes on a gin engine.
func (s *Server) buildRouter() *gin.Engine {
	// We use gin.New() rather than gin.Default() so we can attach our own
	// cubelog-based access logger and recovery handler. gin.Default() writes
	// to stdout and bypasses any logger the operator has configured.
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(requestLogger())
	r.Use(cubeopsRecovery())

	// Health check (no auth); pings Redis so an outage surfaces as 503.
	r.GET("/health", func(c *gin.Context) {
		if err := nodemetric.Ping(c.Request.Context()); err != nil {
			c.String(http.StatusServiceUnavailable, "redis: %v", err)
			return
		}
		c.String(http.StatusOK, "ok")
	})

	// Wire up service layer + handlers.
	authSvc := service.NewAuthService(s.store, s.jm)
	authH := auth.NewHandler(authSvc)
	clusterH := handler.NewClusterHandler(s.cm).WithNodeService(s.nodeSvc)
	storeH := handler.NewStoreHandler(handler.DefaultRegistryClient())
	configH := handler.NewConfigHandler(s.cfg.Bind, 100, s.cfg.JWTSecret != "", s.cfg.SandboxDomain, "cubebox")
	agenthubH := handler.NewAgentHubHandler(s.store, s.cm)
	// SDK handler gets the AgentHubService so that E2B template/snapshot
	// deletions can reverse-sync AgentHub registrations
	sdkH := handler.NewSDKHandler(s.cm).WithAgentHubService(agenthubH.AgentHubService())

	internalH := nmhandler.NewInternalHandler(s.nodeSvc)
	agentH := nmhandler.NewAgentHandler(s.nodeSvc)

	// Public (no auth) routes — login + refresh.
	public := r.Group("/api/v1")
	authH.RegisterPublic(public)

	// Authenticated routes. The session / logout / change-password endpoints
	// are mounted here, behind the JWT middleware.
	authed := r.Group("/api/v1", auth.Middleware(s.jm))
	authH.RegisterAuthed(authed)

	clusterH.Register(authed)
	configH.Register(authed)
	storeH.Register(authed)
	agenthubH.Register(authed)

	// Internal routes — no auth. These endpoints must not be exposed through
	// nginx or a public Bind address. Callers: CubeMaster, cubeopscli.
	internalH.Register(r.Group("/internal/v1"))

	// Internal routes — no auth. These endpoints must not be exposed through
	// nginx or a public Bind address. Callers: Cubelet (register + heartbeat).
	agentH.Register(r.Group("/internal/v1/node-agent"))

	// SDK routes — mounted at both /api/v1/sdk and /api/v1/sdk/v2 because
	// the WebUI and the E2B-compatible clients hit different prefixes.
	sdkGroup := authed.Group("/sdk")
	sdkH.Register(sdkGroup)
	sdkV2Group := authed.Group("/sdk/v2")
	sdkV2Group.GET("/sandboxes", sdkH.ListSandboxes)
	sdkV2Group.GET("/sandboxes/:id/logs", sdkH.GetSandboxLogs)

	return r
}

// requestLogger emits request traces to the stat log.
//
// Convention: handlers own business RetCode (e.g. CubeMaster ret_code); the
// middleware only falls back to HTTP status when rt.RetCode is still zero so
// business codes aren't clobbered by 404/502.
func requestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		requestID := requestIDFromHeader(c)
		caller := callerFromHeader(c)
		// Only /health is a routed liveness endpoint; treating the unrouted
		// /ready as a probe would silently drop its 404 from the logs.
		isProbe := c.Request.URL.Path == "/health"
		if isProbe {
			caller = "probe"
		}
		rt := &cubelog.RequestTrace{
			RequestID:      requestID,
			Action:         c.Request.Method,
			Caller:         caller,
			Callee:         "cubeops",
			CallerIP:       c.ClientIP(),
			CalleeAction:   c.Request.URL.Path,
			CalleeEndpoint: c.Request.Host,
			Timestamp:      start,
		}
		ctx := cubelog.WithRequestTrace(c.Request.Context(), rt)
		ctx = logging.WithLogger(ctx, cubelog.WithContext(ctx))
		c.Request = c.Request.WithContext(ctx)
		c.Header("X-RequestID", requestID)

		defer func() {
			rt.Cost = time.Since(start)
			if isProbe {
				return
			}
			// Only fall back to HTTP status when the handler did not set a
			// business RetCode. This preserves CubeMaster ret_code in -stat.
			if rt.RetCode == 0 {
				rt.RetCode = int64(c.Writer.Status())
			}
			cubelog.Trace(rt)
		}()
		c.Next()
	}
}

// traceFieldPattern is the allowed charset for inbound X-RequestID and
// X-Caller values. Anything outside it falls back to a safe default.
var traceFieldPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// cubeopsRecovery logs panics through cubelog with the inbound RequestID.
// Must be registered after requestLogger. Mirrors gin.Recovery's write guard.
func cubeopsRecovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				stack := debug.Stack()
				logging.G(c.Request.Context()).Errorf(
					"panic recovered: err=%q\n%s", r, stack)
				if !c.Writer.Written() {
					c.AbortWithStatus(http.StatusInternalServerError)
				}
			}
		}()
		c.Next()
	}
}

func requestIDFromHeader(c *gin.Context) string {
	for _, h := range []string{"X-RequestID", "X-Request-ID"} {
		v := strings.TrimSpace(c.GetHeader(h))
		if v != "" && len(v) <= 128 && traceFieldPattern.MatchString(v) {
			return v
		}
	}
	return uuid.NewString()
}

func callerFromHeader(c *gin.Context) string {
	if caller := strings.TrimSpace(c.GetHeader("X-Caller")); caller != "" &&
		len(caller) <= 128 && traceFieldPattern.MatchString(caller) {
		return caller
	}
	return "cubeops-client"
}
