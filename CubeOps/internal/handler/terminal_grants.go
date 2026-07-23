// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

var (
	errTerminalInvalidTarget   = errors.New("invalid terminal target")
	errTerminalPendingLimit    = errors.New("terminal pending grant limit exceeded")
	errTerminalUnknownGrant    = errors.New("unknown terminal grant")
	errTerminalExpiredGrant    = errors.New("expired terminal grant")
	errTerminalBindingMismatch = errors.New("terminal binding cookie mismatch")
	errTerminalActiveLimit     = errors.New("terminal active session limit exceeded")
)

const (
	// Keep these wire-level bounds aligned with CubeMaster terminalprotocol
	// and Cubelet terminalcore (separate Go modules).
	terminalMinCols = 2
	terminalMaxCols = 500
	terminalMinRows = 1
	terminalMaxRows = 200
)

type terminalTarget struct {
	sandboxID   string
	containerID string
	cols        uint16
	rows        uint16
}

func (target terminalTarget) validate() error {
	if target.sandboxID == "" || target.containerID == "" ||
		target.cols < terminalMinCols || target.cols > terminalMaxCols ||
		target.rows < terminalMinRows || target.rows > terminalMaxRows {
		return errTerminalInvalidTarget
	}
	return nil
}

type terminalLimits struct {
	grantTTL         time.Duration
	pendingPerUser   int
	pendingGlobal    int
	activePerUser    int
	activePerSandbox int
	activeGlobal     int
}

func defaultTerminalLimits() terminalLimits {
	return terminalLimits{
		grantTTL:         time.Minute,
		pendingPerUser:   16,
		pendingGlobal:    1024,
		activePerUser:    4,
		activePerSandbox: 8,
		activeGlobal:     128,
	}
}

type issuedTerminalGrant struct {
	token       string
	sessionID   string
	cookieName  string
	cookieValue string
	expiresIn   time.Duration
}

type pendingTerminalGrant struct {
	principal   string
	sessionID   string
	target      terminalTarget
	cookieName  string
	cookieValue string
	expiresAt   time.Time
}

type terminalGrantState struct {
	pending         map[string]pendingTerminalGrant
	activeByUser    map[string]int
	activeBySandbox map[string]int
	activeGlobal    int
}

type terminalGrantStore struct {
	mu     sync.Mutex
	limits terminalLimits
	state  terminalGrantState
}

func newTerminalGrantStore(limits terminalLimits) *terminalGrantStore {
	return &terminalGrantStore{
		limits: limits,
		state: terminalGrantState{
			pending:         make(map[string]pendingTerminalGrant),
			activeByUser:    make(map[string]int),
			activeBySandbox: make(map[string]int),
		},
	}
}

func (store *terminalGrantStore) issue(principal string, target terminalTarget) (issuedTerminalGrant, error) {
	if principal == "" || target.validate() != nil {
		return issuedTerminalGrant{}, errTerminalInvalidTarget
	}
	now := time.Now()
	store.mu.Lock()
	defer store.mu.Unlock()
	for token, grant := range store.state.pending {
		if !grant.expiresAt.After(now) {
			delete(store.state.pending, token)
		}
	}
	pendingForUser := 0
	for _, grant := range store.state.pending {
		if grant.principal == principal {
			pendingForUser++
		}
	}
	if len(store.state.pending) >= store.limits.pendingGlobal || pendingForUser >= store.limits.pendingPerUser {
		return issuedTerminalGrant{}, errTerminalPendingLimit
	}
	token, err := randomTerminalToken()
	if err != nil {
		return issuedTerminalGrant{}, fmt.Errorf("generate terminal grant: %w", err)
	}
	cookieValue, err := randomTerminalToken()
	if err != nil {
		return issuedTerminalGrant{}, fmt.Errorf("generate terminal binding: %w", err)
	}
	sessionID := uuid.NewString()
	cookieName := "cube_terminal_" + strings.ReplaceAll(sessionID, "-", "")
	store.state.pending[token] = pendingTerminalGrant{
		principal: principal, sessionID: sessionID, target: target,
		cookieName: cookieName, cookieValue: cookieValue, expiresAt: now.Add(store.limits.grantTTL),
	}
	return issuedTerminalGrant{token: token, sessionID: sessionID, cookieName: cookieName, cookieValue: cookieValue, expiresIn: store.limits.grantTTL}, nil
}

func randomTerminalToken() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

type terminalSessionLease struct {
	store     *terminalGrantStore
	principal string
	sessionID string
	target    terminalTarget
	once      sync.Once
}

func (store *terminalGrantStore) consume(token string, cookies map[string]string) (*terminalSessionLease, error) {
	now := time.Now()
	store.mu.Lock()
	defer store.mu.Unlock()
	grant, ok := store.state.pending[token]
	if !ok {
		return nil, errTerminalUnknownGrant
	}
	supplied := cookies[grant.cookieName]
	if subtle.ConstantTimeCompare([]byte(grant.cookieValue), []byte(supplied)) != 1 {
		return nil, errTerminalBindingMismatch
	}
	if !grant.expiresAt.After(now) {
		delete(store.state.pending, token)
		return nil, errTerminalExpiredGrant
	}
	delete(store.state.pending, token)
	if store.state.activeGlobal >= store.limits.activeGlobal ||
		store.state.activeByUser[grant.principal] >= store.limits.activePerUser ||
		store.state.activeBySandbox[grant.target.sandboxID] >= store.limits.activePerSandbox {
		return nil, errTerminalActiveLimit
	}
	store.state.activeGlobal++
	store.state.activeByUser[grant.principal]++
	store.state.activeBySandbox[grant.target.sandboxID]++
	return &terminalSessionLease{store: store, principal: grant.principal, sessionID: grant.sessionID, target: grant.target}, nil
}

func (lease *terminalSessionLease) release() {
	if lease == nil || lease.store == nil {
		return
	}
	lease.once.Do(func() {
		store := lease.store
		store.mu.Lock()
		defer store.mu.Unlock()
		store.state.activeGlobal--
		decrementTerminalCount(store.state.activeByUser, lease.principal)
		decrementTerminalCount(store.state.activeBySandbox, lease.target.sandboxID)
	})
}

func decrementTerminalCount(counts map[string]int, key string) {
	if counts[key] <= 1 {
		delete(counts, key)
		return
	}
	counts[key]--
}
