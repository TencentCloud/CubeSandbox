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
// (login + refresh) on the given router group.
func (h *Handler) RegisterPublic(r *gin.RouterGroup) {
	//rate-limit login to protect weak default credentials.
	r.POST("/auth/login", LoginRateLimit(), h.Login)
	r.POST("/auth/refresh", h.Refresh)
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
