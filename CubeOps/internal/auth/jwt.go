// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/service"
)

// tokenTypeAccess / tokenTypeRefresh are the values carried in the
// private "typ" claim to enforce access/refresh token-type isolation.
// Without this, a refresh token (7-day TTL) could be presented to
// VerifyAccessToken and accepted, turning it into a long-lived access
// token.
const (
	tokenTypeAccess  = "access"
	tokenTypeRefresh = "refresh"

	audAccess  = "cubeops:access"  // audience for access tokens
	audRefresh = "cubeops:refresh" // audience for refresh tokens
)

// AccessClaims is the JWT claims for short-lived access tokens.
type AccessClaims struct {
	jwt.RegisteredClaims
	Username string   `json:"username"`
	Role     string   `json:"role"`   // reserved, currently fixed to "admin"
	Scopes   []string `json:"scopes"` // reserved, currently empty
	Typ      string   `json:"typ"`    // token type, always "access"
}

// RefreshClaims is the JWT claims for long-lived refresh tokens.
type RefreshClaims struct {
	jwt.RegisteredClaims
	Username string `json:"username"`
	TokenID  string `json:"tid"`
	Typ      string `json:"typ"` // token type, always "refresh"
}

// JWTManager handles JWT signing and verification.
type JWTManager struct {
	secret     []byte
	accessTTL  time.Duration
	refreshTTL time.Duration
}

// NewJWTManager creates a new JWTManager.
func NewJWTManager(secret string, accessTTL, refreshTTL time.Duration) *JWTManager {
	return &JWTManager{
		secret:     []byte(secret),
		accessTTL:  accessTTL,
		refreshTTL: refreshTTL,
	}
}

// AccessTTL returns the configured access-token TTL.
func (m *JWTManager) AccessTTL() time.Duration { return m.accessTTL }

// RefreshTTL returns the configured refresh-token TTL.
func (m *JWTManager) RefreshTTL() time.Duration { return m.refreshTTL }

// GenerateAccessToken creates a signed JWT access token.
func (m *JWTManager) GenerateAccessToken(username string) (string, error) {
	now := time.Now()
	claims := AccessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(m.accessTTL)),
			IssuedAt:  jwt.NewNumericDate(now),
			Subject:   username,
			Audience:  jwt.ClaimStrings{audAccess},
		},
		Username: username,
		Role:     "admin",
		Scopes:   []string{},
		Typ:      tokenTypeAccess,
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(m.secret)
}

// GenerateRefreshToken creates a signed JWT refresh token.
func (m *JWTManager) GenerateRefreshToken(username string) (string, string, error) {
	now := time.Now()
	tokenID := uuid.New().String()
	claims := RefreshClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(m.refreshTTL)),
			IssuedAt:  jwt.NewNumericDate(now),
			Subject:   username,
			Audience:  jwt.ClaimStrings{audRefresh},
		},
		Username: username,
		TokenID:  tokenID,
		Typ:      tokenTypeRefresh,
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(m.secret)
	if err != nil {
		return "", "", err
	}
	return signed, tokenID, nil
}

// VerifyAccessToken parses and validates an access token. It rejects refresh
// tokens by checking the "typ" claim and the audience, so a long-lived
// refresh token cannot be used as an access token.
func (m *JWTManager) VerifyAccessToken(tokenStr string) (*AccessClaims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &AccessClaims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return m.secret, nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithAudience(audAccess))
	if err != nil {
		return nil, err
	}
	claims, ok := token.Claims.(*AccessClaims)
	if !ok || !token.Valid {
		return nil, errors.New("invalid access token")
	}
	if claims.Typ != tokenTypeAccess {
		return nil, errors.New("not an access token")
	}
	return claims, nil
}

// VerifyRefreshToken parses and validates a refresh token and returns the
// service-layer claim DTO. We return *service.RefreshClaims (instead of
// *RefreshClaims) so the service package can depend on its own types rather
// than on this package's internals. It rejects access tokens via the "typ"
// claim and the audience.
func (m *JWTManager) VerifyRefreshToken(tokenStr string) (*service.RefreshClaims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &RefreshClaims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return m.secret, nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithAudience(audRefresh))
	if err != nil {
		return nil, err
	}
	claims, ok := token.Claims.(*RefreshClaims)
	if !ok || !token.Valid {
		return nil, errors.New("invalid refresh token")
	}
	if claims.Typ != tokenTypeRefresh {
		return nil, errors.New("not a refresh token")
	}
	return &service.RefreshClaims{Username: claims.Username, TokenID: claims.TokenID}, nil
}
