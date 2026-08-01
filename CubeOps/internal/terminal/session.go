// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package terminal

import (
	"context"
	"sync"
	"time"
)

// DefaultIdleTimeout is how long a session may go without user input before
// the registry kills the PTY and closes any attached WebSocket.
const DefaultIdleTimeout = 30 * time.Minute

// Session is one PTY inside one sandbox. A session survives WebSocket drops:
// when the socket dies abnormally the session is detached (PTY kept alive)
// and the frontend can reattach via envd Connect with the recorded PID.
type Session struct {
	ID        string
	SandboxID string
	Username  string
	ClientIP  string
	PID       int
	CreatedAt time.Time

	lastActive time.Time
	attached   bool
	// closeWS force-closes the currently attached WebSocket (set while
	// attached). Invoked by the idle sweeper outside the registry lock.
	closeWS func(reason string)
}

// Registry tracks live terminal sessions and reaps idle ones.
type Registry struct {
	client      *Client
	idleTimeout time.Duration
	now         func() time.Time // injectable for tests

	mu       sync.Mutex
	sessions map[string]*Session
	stop     chan struct{}
	stopOnce sync.Once
}

// NewRegistry creates a session registry. Call Start to begin idle sweeping.
func NewRegistry(client *Client, idleTimeout time.Duration) *Registry {
	if idleTimeout <= 0 {
		idleTimeout = DefaultIdleTimeout
	}
	return &Registry{
		client:      client,
		idleTimeout: idleTimeout,
		now:         time.Now,
		sessions:    make(map[string]*Session),
		stop:        make(chan struct{}),
	}
}

// IdleTimeout returns the configured idle timeout.
func (r *Registry) IdleTimeout() time.Duration { return r.idleTimeout }

// Start launches the idle sweeper goroutine.
func (r *Registry) Start() {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				r.sweep()
			case <-r.stop:
				return
			}
		}
	}()
}

// Stop halts the idle sweeper.
func (r *Registry) Stop() { r.stopOnce.Do(func() { close(r.stop) }) }

// Add registers a new attached session.
func (r *Registry) Add(s *Session, closeWS func(reason string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s.lastActive = r.now()
	s.attached = true
	s.closeWS = closeWS
	r.sessions[s.ID] = s
}

// Remove deletes a session (its PTY is gone or being killed).
func (r *Registry) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, id)
}

// Touch records user activity on a session.
func (r *Registry) Touch(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sessions[id]; ok {
		s.lastActive = r.now()
	}
}

// Detach marks a session's WebSocket as gone while keeping the PTY alive for
// a later reconnect. The idle sweeper still applies.
func (r *Registry) Detach(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sessions[id]; ok {
		s.attached = false
		s.closeWS = nil
	}
}

// Reattach claims a detached session for a new WebSocket, identified by
// sandbox + PID (what the frontend has). It fails if the session is unknown
// or still attached elsewhere.
func (r *Registry) Reattach(sandboxID string, pid int, closeWS func(reason string)) *Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.sessions {
		if s.SandboxID == sandboxID && s.PID == pid && !s.attached {
			s.attached = true
			s.closeWS = closeWS
			s.lastActive = r.now()
			return s
		}
	}
	return nil
}

// CountForSandbox returns the number of live sessions for a sandbox.
func (r *Registry) CountForSandbox(sandboxID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, s := range r.sessions {
		if s.SandboxID == sandboxID {
			n++
		}
	}
	return n
}

// sweep kills PTYs idle beyond the timeout and closes their sockets.
func (r *Registry) sweep() {
	r.mu.Lock()
	var idle []*Session
	cutoff := r.now().Add(-r.idleTimeout)
	for id, s := range r.sessions {
		if s.lastActive.Before(cutoff) {
			idle = append(idle, s)
			delete(r.sessions, id)
		}
	}
	r.mu.Unlock()

	for _, s := range idle {
		if s.closeWS != nil {
			s.closeWS("idle_timeout")
		}
		ctx, cancel := context.WithTimeout(context.Background(), unaryTimeout)
		_ = r.client.Kill(ctx, s.SandboxID, s.PID)
		cancel()
		auditEnd(s, "idle_timeout")
	}
}
