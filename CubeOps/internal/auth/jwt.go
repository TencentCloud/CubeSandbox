// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"errors"
	"fmt"
	"strings"
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
	tokenTypeAccess   = "access"
	tokenTypeRefresh  = "refresh"
	tokenTypeTerminal = "terminal"

	audAccess   = "cubeops:access"   // audience for access tokens
	audRefresh  = "cubeops:refresh"  // audience for refresh tokens
	audTerminal = "cubeops:terminal" // audience for short-lived Web Terminal tickets
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

// TerminalClaims is carried by the short-lived ticket used to upgrade a
// browser connection to a Web Terminal WebSocket. A separate audience and
// token type prevent access or refresh tokens from being accepted as terminal
// tickets. SandboxID binds the ticket to exactly one sandbox.
type TerminalClaims struct {
	jwt.RegisteredClaims
	Username  string `json:"username"`
	SandboxID string `json:"sandbox_id"`
	Typ       string `json:"typ"`
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

// GenerateTerminalTicket creates a short-lived ticket for a single sandbox.
// The ticket is transported in Sec-WebSocket-Protocol rather than a URL query
// parameter so it is not copied into access logs, browser history, or referrer
// headers.
func (m *JWTManager) GenerateTerminalTicket(username, sandboxID string, ttl time.Duration) (string, error) {
	if strings.TrimSpace(username) == "" || strings.TrimSpace(sandboxID) == "" {
		return "", errors.New("terminal ticket requires username and sandbox ID")
	}
	if ttl <= 0 {
		return "", errors.New("terminal ticket TTL must be positive")
	}
	now := time.Now()
	claims := TerminalClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now.Add(-5 * time.Second)),
			Subject:   username,
			Audience:  jwt.ClaimStrings{audTerminal},
			ID:        uuid.New().String(),
		},
		Username:  username,
		SandboxID: sandboxID,
		Typ:       tokenTypeTerminal,
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(m.secret)
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

// VerifyTerminalTicket parses and validates a Web Terminal ticket. Access and
// refresh JWTs are rejected by both the dedicated audience and typ claim.
func (m *JWTManager) VerifyTerminalTicket(tokenStr string) (*TerminalClaims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &TerminalClaims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return m.secret, nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithAudience(audTerminal))
	if err != nil {
		return nil, err
	}
	claims, ok := token.Claims.(*TerminalClaims)
	if !ok || !token.Valid {
		return nil, errors.New("invalid terminal ticket")
	}
	if claims.Typ != tokenTypeTerminal {
		return nil, errors.New("not a terminal ticket")
	}
	if strings.TrimSpace(claims.Username) == "" || strings.TrimSpace(claims.SandboxID) == "" {
		return nil, errors.New("terminal ticket is missing required claims")
	}
	return claims, nil
}
