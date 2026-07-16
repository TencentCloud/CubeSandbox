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

// AccessClaims is the JWT claims for short-lived access tokens.
type AccessClaims struct {
	jwt.RegisteredClaims
	Username string   `json:"username"`
	Role     string   `json:"role"`   // reserved, currently fixed to "admin"
	Scopes   []string `json:"scopes"` // reserved, currently empty
}

// RefreshClaims is the JWT claims for long-lived refresh tokens.
type RefreshClaims struct {
	jwt.RegisteredClaims
	Username string `json:"username"`
	TokenID  string `json:"tid"`
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
		},
		Username: username,
		Role:     "admin",
		Scopes:   []string{},
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
		},
		Username: username,
		TokenID:  tokenID,
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(m.secret)
	if err != nil {
		return "", "", err
	}
	return signed, tokenID, nil
}

// VerifyAccessToken parses and validates an access token.
func (m *JWTManager) VerifyAccessToken(tokenStr string) (*AccessClaims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &AccessClaims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return m.secret, nil
	})
	if err != nil {
		return nil, err
	}
	claims, ok := token.Claims.(*AccessClaims)
	if !ok || !token.Valid {
		return nil, errors.New("invalid access token")
	}
	return claims, nil
}

// VerifyRefreshToken parses and validates a refresh token and returns the
// service-layer claim DTO. We return *service.RefreshClaims (instead of
// *RefreshClaims) so the service package can depend on its own types rather
// than on this package's internals.
func (m *JWTManager) VerifyRefreshToken(tokenStr string) (*service.RefreshClaims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &RefreshClaims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return m.secret, nil
	})
	if err != nil {
		return nil, err
	}
	claims, ok := token.Claims.(*RefreshClaims)
	if !ok || !token.Valid {
		return nil, errors.New("invalid refresh token")
	}
	return &service.RefreshClaims{Username: claims.Username, TokenID: claims.TokenID}, nil
}
