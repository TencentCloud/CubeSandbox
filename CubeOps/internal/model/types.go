// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package model

// LoginRequest is the request body for POST /auth/login.
type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// LoginResponse is the response body for POST /auth/login.
type LoginResponse struct {
	AccessToken   string `json:"accessToken"`
	RefreshToken  string `json:"refreshToken"`
	Username      string `json:"username"`
	ExpiresInSecs int64  `json:"expiresInSecs"`
}

// SessionResponse is the response body for GET /auth/session.
type SessionResponse struct {
	AuthRequired  bool   `json:"authRequired"`
	Authenticated bool   `json:"authenticated"`
	Username      string `json:"username,omitempty"`
}

// ChangePasswordRequest is the request body for POST /auth/change-password.
type ChangePasswordRequest struct {
	Username    string `json:"username"`
	OldPassword string `json:"oldPassword"`
	NewPassword string `json:"newPassword"`
}

// RefreshRequest is the request body for POST /auth/refresh.
type RefreshRequest struct {
	RefreshToken string `json:"refreshToken"`
}

// RefreshResponse is the response body for POST /auth/refresh.
type RefreshResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
}

// APIError is a generic error response.
type APIError struct {
	Error string `json:"error"`
}
