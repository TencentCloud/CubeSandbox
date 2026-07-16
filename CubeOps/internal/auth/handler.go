// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"encoding/json"
	"net/http"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/crypto"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/model"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
)

const sessionTTLName = "session"

// Handler holds dependencies for auth HTTP handlers.
type Handler struct {
	store *store.Store
	jm    *JWTManager
}

// NewHandler creates a new auth handler.
func NewHandler(s *store.Store, jm *JWTManager) *Handler {
	return &Handler{store: s, jm: jm}
}

// Login handles POST /auth/login.
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var req model.LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Username == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "username and password are required")
		return
	}

	stored, err := h.store.GetUserPassword(r.Context(), req.Username)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read user")
		return
	}
	if !crypto.VerifyPassword(stored, req.Password) {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	accessToken, err := h.jm.GenerateAccessToken(req.Username)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate access token")
		return
	}
	refreshToken, _, err := h.jm.GenerateRefreshToken(req.Username)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate refresh token")
		return
	}

	writeJSON(w, http.StatusOK, model.LoginResponse{
		AccessToken:   accessToken,
		RefreshToken:  refreshToken,
		Username:      req.Username,
		ExpiresInSecs: int64(h.jm.accessTTL.Seconds()),
	})
}

// Session handles GET /auth/session.
func (h *Handler) Session(w http.ResponseWriter, r *http.Request) {
	username := UsernameFromContext(r.Context())
	writeJSON(w, http.StatusOK, model.SessionResponse{
		AuthRequired: true,
		Authenticated: username != "",
		Username:     username,
	})
}

// Logout handles POST /auth/logout.
func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	// JWT is stateless; client simply discards the token.
	// If we add Redis blacklist later, we'd invalidate the token here.
	w.WriteHeader(http.StatusNoContent)
}

// ChangePassword handles POST /auth/change-password.
//
// The target username is taken exclusively from the JWT-authenticated
// request context (UsernameFromContext). Any "username" field in the
// request body is ignored to prevent IDOR — a caller must not be able
// to change another user's password by passing that user's name in the
// payload.
func (h *Handler) ChangePassword(w http.ResponseWriter, r *http.Request) {
	var req model.ChangePasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	username := UsernameFromContext(r.Context())
	if username == "" {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if req.OldPassword == "" || req.NewPassword == "" {
		writeError(w, http.StatusBadRequest, "oldPassword and newPassword are required")
		return
	}
	if len(req.NewPassword) < 4 {
		writeError(w, http.StatusBadRequest, "new password must be at least 4 characters")
		return
	}

	stored, err := h.store.GetUserPassword(r.Context(), username)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read user")
		return
	}
	if !crypto.VerifyPassword(stored, req.OldPassword) {
		writeError(w, http.StatusUnauthorized, "current password is incorrect or user not found")
		return
	}

	newHash, err := crypto.HashPassword(req.NewPassword)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to hash password")
		return
	}
	if err := h.store.SetUserPassword(r.Context(), username, newHash); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update password")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Refresh handles POST /auth/refresh.
func (h *Handler) Refresh(w http.ResponseWriter, r *http.Request) {
	var req model.RefreshRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.RefreshToken == "" {
		writeError(w, http.StatusBadRequest, "refreshToken is required")
		return
	}

	claims, err := h.jm.VerifyRefreshToken(req.RefreshToken)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid or expired refresh token")
		return
	}

	accessToken, err := h.jm.GenerateAccessToken(claims.Username)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate access token")
		return
	}
	writeJSON(w, http.StatusOK, model.RefreshResponse{AccessToken: accessToken})
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, model.APIError{Error: msg})
}
