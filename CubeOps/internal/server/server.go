// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/auth"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/config"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/cubemaster"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/handler"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/httputil"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/logging"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/service"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
	cubelog "github.com/tencentcloud/CubeSandbox/cubelog"
)

// WebhookStatus is the webhook subsystem health surface consumed by probes.
type WebhookStatus interface {
	Started() bool
	Healthy(ctx context.Context) bool
}

// Server is the CubeOps HTTP server.
type Server struct {
	cfg     *config.Config
	store   *store.Store
	jm      *auth.JWTManager
	httpSrv *http.Server
	cm      *cubemaster.Client
	webhook WebhookStatus

	whMu       sync.Mutex
	whFailures int
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

// SetWebhookStatus attaches the webhook subsystem (nil when disabled) so the
// readiness and health endpoints can report it.
func (s *Server) SetWebhookStatus(w WebhookStatus) { s.webhook = w }

// Start begins listening for HTTP requests.
func (s *Server) Start() error {
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

	// Health check (no auth) — defined at the root rather than under /api/v1
	// because external load balancers and k8s probes hit it without a prefix.
	r.GET("/health", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})
	// Prometheus scrape endpoint (no auth, same port).
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// Readiness: the HTTP status is driven ONLY by DB connectivity so a
	// webhook subsystem fault cannot take the management API off the load
	// balancer; webhook state is reported in the body for monitors / smart
	// gateways.
	r.GET("/readyz", s.readyz)
	// Dedicated webhook health probe (no auth, alert-only semantics).
	r.GET("/webhook/healthz", s.webhookHealthz)

	// Wire up service layer + handlers.
	authSvc := service.NewAuthService(s.store, s.jm)
	authH := auth.NewHandler(authSvc)
	clusterH := handler.NewClusterHandler(s.cm)
	storeH := handler.NewStoreHandler(handler.DefaultRegistryClient())
	configH := handler.NewConfigHandler(s.cfg.Bind, 100, s.cfg.JWTSecret != "", s.cfg.SandboxDomain, "cubebox")
	agenthubH := handler.NewAgentHubHandler(s.store, s.cm)
	webhookH := handler.NewWebhookHandler(service.NewWebhookService(s.store), s.cfg.Webhook.Enabled)
	// SDK handler gets the AgentHubService so that E2B template/snapshot
	// deletions can reverse-sync AgentHub registrations
	sdkH := handler.NewSDKHandler(s.cm).WithAgentHubService(agenthubH.AgentHubService())

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
	webhookH.Register(authed)

	// SDK routes — mounted at both /api/v1/sdk and /api/v1/sdk/v2 because
	// the WebUI and the E2B-compatible clients hit different prefixes.
	sdkGroup := authed.Group("/sdk")
	sdkH.Register(sdkGroup)
	sdkV2Group := authed.Group("/sdk/v2")
	sdkV2Group.GET("/sandboxes", sdkH.ListSandboxes)
	sdkV2Group.GET("/sandboxes/:id/logs", sdkH.GetSandboxLogs)

	return r
}

// readyz reports DB connectivity as the authoritative status and includes the
// webhook subsystem state as a JSON field (monitor-only, does not gate LB).
func (s *Server) readyz(c *gin.Context) {
	ctx := c.Request.Context()
	dbOK := s.store.DB().WithContext(ctx).Exec("SELECT 1").Error == nil
	status := http.StatusOK
	if !dbOK {
		status = http.StatusServiceUnavailable
	}
	webhookReady := false
	if s.webhook != nil {
		webhookReady = s.webhook.Healthy(ctx)
	}
	httputil.WriteJSON(c, status, gin.H{
		"db":            map[string]string{"status": readyStatus(dbOK)},
		"webhook_ready": webhookReady,
	})
}

// webhookHealthz is an alert-only probe: it returns 200 with a "degraded"
// body during a short tolerance window (3 consecutive failures) and only
// flips to 503 after the window. It is deliberately NOT wired to k8s probes
// (see chart comments) so a Redis blip never restarts or drains the pod.
func (s *Server) webhookHealthz(c *gin.Context) {
	ctx := c.Request.Context()
	if !s.cfg.Webhook.Enabled {
		httputil.WriteJSON(c, http.StatusOK, gin.H{"webhook": "disabled"})
		return
	}
	ready := s.webhook != nil && s.webhook.Healthy(ctx)
	s.whMu.Lock()
	defer s.whMu.Unlock()
	if !ready {
		s.whFailures++
		if s.whFailures < 3 {
			httputil.WriteJSON(c, http.StatusOK, gin.H{
				"webhook": "degraded", "consecutive_failures": s.whFailures,
			})
			return
		}
		httputil.WriteJSON(c, http.StatusServiceUnavailable, gin.H{
			"webhook": "not_ready", "consecutive_failures": s.whFailures,
		})
		return
	}
	s.whFailures = 0
	httputil.WriteJSON(c, http.StatusOK, gin.H{"webhook": "ready"})
}

func readyStatus(ok bool) string {
	if ok {
		return "ok"
	}
	return "unavailable"
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
