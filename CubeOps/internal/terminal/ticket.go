// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package terminal

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Browsers cannot set an Authorization header on a WebSocket handshake, so the
// terminal uses a dedicated one-time ticket instead of putting the login JWT
// in the query string (where it would land in access logs and history). The
// ticket is a short-lived JWT with its own audience — it cannot be replayed
// against normal API endpoints, and a login token cannot open a terminal.
const (
	ticketAudience = "cubeops:terminal"
	ticketType     = "terminal"

	// DefaultTicketTTL bounds the window between ticket issuance and the
	// WebSocket handshake; the frontend connects immediately, so 30s is ample.
	DefaultTicketTTL = 30 * time.Second
)

// TicketClaims are the claims carried by a terminal ticket.
type TicketClaims struct {
	jwt.RegisteredClaims
	Username  string `json:"username"`
	SandboxID string `json:"sandboxID"`
	Typ       string `json:"typ"`
}

// TicketManager issues and redeems one-time terminal tickets.
type TicketManager struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time // injectable for tests

	mu   sync.Mutex
	used map[string]time.Time // jti -> ticket expiry, kept until expiry passes
}

// NewTicketManager creates a TicketManager signing with the same secret as the
// login JWTs (audience isolation keeps the token classes apart).
func NewTicketManager(secret string, ttl time.Duration) *TicketManager {
	if ttl <= 0 {
		ttl = DefaultTicketTTL
	}
	return &TicketManager{
		secret: []byte(secret),
		ttl:    ttl,
		now:    time.Now,
		used:   make(map[string]time.Time),
	}
}

// Issue signs a ticket authorizing username to open a terminal into sandboxID.
func (t *TicketManager) Issue(username, sandboxID string) (string, error) {
	now := t.now()
	claims := TicketClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        uuid.New().String(),
			ExpiresAt: jwt.NewNumericDate(now.Add(t.ttl)),
			IssuedAt:  jwt.NewNumericDate(now),
			Subject:   username,
			Audience:  jwt.ClaimStrings{ticketAudience},
		},
		Username:  username,
		SandboxID: sandboxID,
		Typ:       ticketType,
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.secret)
}

// Redeem validates a ticket and consumes it; a second Redeem of the same
// ticket fails even inside the TTL window.
func (t *TicketManager) Redeem(tokenStr string) (*TicketClaims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &TicketClaims{}, func(tok *jwt.Token) (interface{}, error) {
		if _, ok := tok.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", tok.Header["alg"])
		}
		return t.secret, nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithAudience(ticketAudience), jwt.WithTimeFunc(t.now))
	if err != nil {
		return nil, fmt.Errorf("invalid terminal ticket: %w", err)
	}
	claims, ok := token.Claims.(*TicketClaims)
	if !ok || !token.Valid || claims.Typ != ticketType {
		return nil, errors.New("not a terminal ticket")
	}
	if claims.ID == "" {
		return nil, errors.New("terminal ticket missing jti")
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	// Drop consumed entries whose tickets have expired anyway; the map stays
	// bounded by the number of tickets issued per TTL window.
	for jti, exp := range t.used {
		if now.After(exp) {
			delete(t.used, jti)
		}
	}
	if _, seen := t.used[claims.ID]; seen {
		return nil, errors.New("terminal ticket already used")
	}
	exp := now.Add(t.ttl)
	if claims.ExpiresAt != nil {
		exp = claims.ExpiresAt.Time
	}
	t.used[claims.ID] = exp
	return claims, nil
}
