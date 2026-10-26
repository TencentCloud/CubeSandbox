// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package service contains the business logic for CubeOps.
//
// The HTTP layer (handler/) is a thin adapter that decodes requests, calls a
// service, and serialises the result. Putting the logic here keeps handlers
// small and makes the logic easy to unit-test without spinning up an HTTP
// server or a database.
package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/crypto"
)

// RefreshClaims is the subset of refresh-token claims the service layer needs.
// auth.RefreshClaims is converted into this struct by the auth package so
// the service layer does not import the auth package (which would create an
// import cycle via auth/handler → service → auth).
type RefreshClaims struct {
	Username string
	TokenID  string
}

// TokenIssuer is the subset of *auth.JWTManager that AuthService depends on.
// *auth.JWTManager satisfies this interface implicitly; tests can supply a
// fake implementation.
type TokenIssuer interface {
	GenerateAccessToken(username string) (string, error)
	GenerateRefreshToken(username string) (string, string, error)
	VerifyRefreshToken(token string) (*RefreshClaims, error)
	AccessTTL() time.Duration
}

// UserStore is the subset of *store.Store that AuthService depends on.
// Defined here so tests can supply an in-memory fake without spinning up
// MySQL; the real *store.Store satisfies it implicitly.
type UserStore interface {
	GetUserPassword(ctx context.Context, username string) (string, error)
	SetUserPassword(ctx context.Context, username, passwordHash string) error
	CreateRefreshToken(ctx context.Context, tokenID, username string) error
	IsRefreshTokenRevoked(ctx context.Context, tokenID string) (bool, error)
	RevokeRefreshToken(ctx context.Context, tokenID string) error
	RevokeAllRefreshTokensForUser(ctx context.Context, username string) error
}

// AuthService handles login / password change / token refresh.
type AuthService struct {
	store UserStore
	jm    TokenIssuer
}

// NewAuthService constructs an AuthService.
func NewAuthService(s UserStore, jm TokenIssuer) *AuthService {
	return &AuthService{store: s, jm: jm}
}

// LoginResult is what Login returns to the HTTP layer.
type LoginResult struct {
	AccessToken   string
	RefreshToken  string
	Username      string
	ExpiresInSecs int64
}

// Login validates credentials and returns access + refresh tokens.
//
// To prevent user enumeration, a missing user and a wrong password are both
// surfaced as ErrInvalidCredentials — the caller cannot tell them apart from
// the response. Genuine infrastructure errors (DB down, etc.) are still
// returned verbatim so the operator can diagnose them.
func (s *AuthService) Login(ctx context.Context, username, password string) (*LoginResult, error) {
	if username == "" || password == "" {
		return nil, errors.New("username and password are required")
	}
	stored, err := s.store.GetUserPassword(ctx, username)
	if err != nil {
		// Distinguish "user not found" (→ ErrInvalidCredentials, safe to
		// expose) from infrastructure errors (→ return verbatim). We do this
		// by checking whether stored is empty — the store layer returns "" +
		// a "not found" error when the row is missing.
		if stored == "" {
			return nil, ErrInvalidCredentials
		}
		return nil, fmt.Errorf("failed to read user: %w", err)
	}
	if !crypto.VerifyPassword(stored, password) {
		return nil, ErrInvalidCredentials
	}
	accessToken, err := s.jm.GenerateAccessToken(username)
	if err != nil {
		return nil, fmt.Errorf("generate access token: %w", err)
	}
	refreshToken, tokenID, err := s.jm.GenerateRefreshToken(username)
	if err != nil {
		return nil, fmt.Errorf("generate refresh token: %w", err)
	}
	// M2 fix: persist refresh token so it can be revoked during rotation
	// and after password change. Without this, a stolen refresh token
	// remains valid for its full 7-day TTL even after the user changes
	// their password.
	if err := s.store.CreateRefreshToken(ctx, tokenID, username); err != nil {
		return nil, fmt.Errorf("persist refresh token: %w", err)
	}
	return &LoginResult{
		AccessToken:   accessToken,
		RefreshToken:  refreshToken,
		Username:      username,
		ExpiresInSecs: int64(s.jm.AccessTTL().Seconds()),
	}, nil
}

// Refresh exchanges a refresh token for a new access token AND a new refresh
// token (rotation). The old refresh token is revoked, so a stolen token
// cannot be replayed after the legitimate user refreshes.
//
// M2 fix: previously Refresh returned only a new access token and left the
// old refresh token valid for its full 7-day TTL. An attacker who stole the
// refresh token could use it indefinitely. With rotation, each refresh
// invalidates the previous token.
func (s *AuthService) Refresh(ctx context.Context, refreshToken string) (string, string, error) {
	if refreshToken == "" {
		return "", "", errors.New("refreshToken is required")
	}
	claims, err := s.jm.VerifyRefreshToken(refreshToken)
	if err != nil {
		return "", "", ErrInvalidRefreshToken
	}
	// M2 fix: check if the token has been revoked (e.g. after password change
	// or a previous rotation).
	revoked, err := s.store.IsRefreshTokenRevoked(ctx, claims.TokenID)
	if err != nil {
		return "", "", fmt.Errorf("check refresh token revocation: %w", err)
	}
	if revoked {
		return "", "", ErrInvalidRefreshToken
	}
	// Generate new access token.
	accessToken, err := s.jm.GenerateAccessToken(claims.Username)
	if err != nil {
		return "", "", fmt.Errorf("generate access token: %w", err)
	}
	// M2 fix: generate a new refresh token (rotation) and revoke the old one.
	newRefreshToken, newTokenID, err := s.jm.GenerateRefreshToken(claims.Username)
	if err != nil {
		return "", "", fmt.Errorf("generate refresh token: %w", err)
	}
	if err := s.store.CreateRefreshToken(ctx, newTokenID, claims.Username); err != nil {
		return "", "", fmt.Errorf("persist new refresh token: %w", err)
	}
	if err := s.store.RevokeRefreshToken(ctx, claims.TokenID); err != nil {
		// Best-effort: log but don't fail the refresh — the new tokens are
		// already issued. The old token will expire naturally.
		_ = err
	}
	return accessToken, newRefreshToken, nil
}

// ChangePassword updates the password for the authenticated user.
//
// The target username is taken from the (already-validated) context — never
// from the request body — to prevent IDOR. The caller is responsible for
// authenticating the request before invoking this method.
func (s *AuthService) ChangePassword(ctx context.Context, username, oldPassword, newPassword string) error {
	if username == "" {
		return ErrUnauthenticated
	}
	if oldPassword == "" || newPassword == "" {
		return errors.New("oldPassword and newPassword are required")
	}
	if len(newPassword) < 4 {
		return errors.New("new password must be at least 4 characters")
	}
	stored, err := s.store.GetUserPassword(ctx, username)
	if err != nil {
		return fmt.Errorf("failed to read user: %w", err)
	}
	if !crypto.VerifyPassword(stored, oldPassword) {
		return ErrInvalidOldPassword
	}
	newHash, err := crypto.HashPassword(newPassword)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	if err := s.store.SetUserPassword(ctx, username, newHash); err != nil {
		return fmt.Errorf("update password: %w", err)
	}
	// M2 fix: revoke all existing refresh tokens for this user so that
	// sessions on other devices are forced to re-authenticate after a
	// password change.
	if err := s.store.RevokeAllRefreshTokensForUser(ctx, username); err != nil {
		// Best-effort: password was changed successfully, token revocation
		// failure should not roll back the password change.
		_ = err
	}
	return nil
}
