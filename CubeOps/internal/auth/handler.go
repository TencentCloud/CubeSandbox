// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/httputil"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/model"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/service"
)

// Handler holds dependencies for auth HTTP handlers.
//
// It is a thin adapter over service.AuthService — it decodes the request,
// delegates to the service, and serialises the result. Business logic lives
// in the service layer where it is easy to unit-test.
type Handler struct {
	svc *service.AuthService
}

// NewHandler creates a new auth handler.
func NewHandler(svc *service.AuthService) *Handler {
	return &Handler{svc: svc}
}

// RegisterPublic installs the auth routes that don't require a valid JWT
// (login + refresh + the CubeAPI auth callback) on the given router group.
func (h *Handler) RegisterPublic(r *gin.RouterGroup) {
	//rate-limit login to protect weak default credentials.
	r.POST("/auth/login", LoginRateLimit(), h.Login)
	r.POST("/auth/refresh", h.Refresh)
	r.POST("/auth/verify", h.Verify)
}

// RegisterAuthed installs the auth routes that require a valid JWT
// (session / logout / change-password) on the given router group.
func (h *Handler) RegisterAuthed(r *gin.RouterGroup) {
	r.GET("/auth/session", h.Session)
	r.POST("/auth/logout", h.Logout)
	r.POST("/auth/change-password", h.ChangePassword)
}

// Login handles POST /auth/login.
func (h *Handler) Login(c *gin.Context) {
	var req model.LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httputil.WriteError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	res, err := h.svc.Login(c.Request.Context(), req.Username, req.Password)
	if err != nil {
		// For security, do not distinguish "user not found" from "wrong password"
		// to the caller.
		if errors.Is(err, service.ErrInvalidCredentials) {
			//record the failure for rate-limiting.
			markLoginFailure(c)
			httputil.WriteError(c, http.StatusUnauthorized, "invalid credentials")
			return
		}
		httputil.WriteError(c, http.StatusInternalServerError, err.Error())
		return
	}
	httputil.WriteJSON(c, http.StatusOK, model.LoginResponse{
		AccessToken:   res.AccessToken,
		RefreshToken:  res.RefreshToken,
		Username:      res.Username,
		ExpiresInSecs: res.ExpiresInSecs,
	})
}

// Session handles GET /auth/session.
func (h *Handler) Session(c *gin.Context) {
	username := c.GetString("username")
	httputil.WriteJSON(c, http.StatusOK, model.SessionResponse{
		AuthRequired:  true,
		Authenticated: username != "",
		Username:      username,
	})
}

// Logout handles POST /auth/logout.
//
// JWT is stateless; the client discards the token. If we add a Redis
// blacklist later we'd invalidate the token here.
func (h *Handler) Logout(c *gin.Context) {
	httputil.WriteNoContent(c)
}

// ChangePassword handles POST /auth/change-password.
//
// The target username is taken exclusively from the JWT-authenticated request
// context, never from the request body — to prevent IDOR.
func (h *Handler) ChangePassword(c *gin.Context) {
	var req model.ChangePasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httputil.WriteError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	username := c.GetString("username")
	err := h.svc.ChangePassword(c.Request.Context(), username, req.OldPassword, req.NewPassword)
	switch {
	case err == nil:
		httputil.WriteNoContent(c)
	case errors.Is(err, service.ErrUnauthenticated):
		httputil.WriteError(c, http.StatusUnauthorized, "authentication required")
	case errors.Is(err, service.ErrInvalidOldPassword):
		httputil.WriteError(c, http.StatusUnauthorized, "current password is incorrect or user not found")
	default:
		// Validation errors and DB errors share the 500 path here; finer
		// mapping can be added by wrapping with sentinel errors if needed.
		httputil.WriteError(c, http.StatusInternalServerError, err.Error())
	}
}

// Verify handles POST /auth/verify — the auth-callback endpoint that
// CubeAPI (via AUTH_CALLBACK_URL) calls to authenticate requests it proxies,
// most notably the WebUI terminal WebSocket (see docs/guide/authentication.md).
//
// The endpoint authenticates only: CubeOps has a single admin identity and
// makes no per-path/per-method authorization decision, so the X-Request-Path
// and X-Request-Method headers CubeAPI forwards are accepted but ignored.
// X-API-Key is not supported — CubeOps only recognises its own JWTs, so a
// request carrying only X-API-Key is rejected like a missing token.
//
// On success it answers 200 with an empty JSON body and the authenticated
// username in the X-Auth-User response header (consumed by CubeAPI for
// terminal audit attribution). On any failure it answers 401 with a generic
// message that does not distinguish a missing token from an invalid one.
//
// No rate limiter is attached: LoginRateLimit is failure-driven and only the
// Login handler records failures, so installing it here would be a no-op.
// Unlike password guessing against /auth/login, brute-forcing a signed JWT
// is not a realistic threat.
func (h *Handler) Verify(c *gin.Context) {
	tokenStr := extractBearerToken(c.GetHeader("Authorization"))
	username, err := h.svc.VerifyAccessToken(c.Request.Context(), tokenStr)
	if err != nil {
		httputil.WriteError(c, http.StatusUnauthorized, "missing or invalid credentials")
		return
	}
	c.Header("X-Auth-User", username)
	httputil.WriteJSON(c, http.StatusOK, gin.H{})
}

// Refresh handles POST /auth/refresh.
func (h *Handler) Refresh(c *gin.Context) {
	var req model.RefreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httputil.WriteError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	accessToken, newRefreshToken, err := h.svc.Refresh(c.Request.Context(), req.RefreshToken)
	if err != nil {
		if errors.Is(err, service.ErrInvalidRefreshToken) {
			httputil.WriteError(c, http.StatusUnauthorized, "invalid or expired refresh token")
			return
		}
		httputil.WriteError(c, http.StatusInternalServerError, err.Error())
		return
	}
	httputil.WriteJSON(c, http.StatusOK, model.RefreshResponse{
		AccessToken:  accessToken,
		RefreshToken: newRefreshToken,
	})
}
