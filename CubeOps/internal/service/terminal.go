// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	cubesandbox "github.com/tencentcloud/CubeSandbox/sdk/go"
)

var (
	ErrTerminalTicketInvalid = errors.New("terminal ticket is invalid or expired")
	ErrTerminalSessionBusy   = errors.New("terminal session is already attached")
	ErrTerminalSessionGone   = errors.New("terminal session is no longer available")
	ErrTerminalSessionLimit  = errors.New("terminal session limit reached")
)

// TerminalTicket is a short-lived, single-use authorization grant. It carries
// only server-side state; the random token is the sole value sent in the
// WebSocket URL.
type TerminalTicket struct {
	Token       string
	Username    string
	SandboxID   string
	ContainerID string
	EnvdPort    int
	SessionID   string
	Rows        int
	Cols        int
	ExpiresAt   time.Time
}

type terminalProcess interface {
	PID() int
	Output() <-chan []byte
	ExitCode() (int, bool)
	ErrorMessage() string
	SendStdin(context.Context, []byte) error
	Resize(context.Context, cubesandbox.PtySize) error
	Disconnect() error
	Kill(context.Context) (bool, error)
}

type terminalBackend interface {
	Start(context.Context, string, int, int, int) (terminalProcess, error)
	Connect(context.Context, string, int, int) (terminalProcess, error)
	Close() error
}

type sdkTerminalBackend struct {
	client *cubesandbox.Client
}

func newSDKTerminalBackend(proxyURL, sandboxDomain string) (*sdkTerminalBackend, error) {
	parsed, err := url.Parse(proxyURL)
	if err != nil || parsed.Scheme == "" || parsed.Hostname() == "" {
		return nil, fmt.Errorf("invalid sandbox proxy URL %q", proxyURL)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("sandbox proxy URL must use http or https")
	}
	port := 80
	if parsed.Scheme == "https" {
		port = 443
	}
	if parsed.Port() != "" {
		port, err = strconv.Atoi(parsed.Port())
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("invalid sandbox proxy URL port %q", parsed.Port())
		}
	}
	client := cubesandbox.NewClient(cubesandbox.Config{
		ProxyNodeIP:    parsed.Hostname(),
		ProxyPortHTTP:  port,
		ProxyScheme:    parsed.Scheme,
		SandboxDomain:  sandboxDomain,
		RequestTimeout: 10 * time.Second,
	})
	return &sdkTerminalBackend{client: client}, nil
}

func (b *sdkTerminalBackend) Start(ctx context.Context, sandboxID string, envdPort, rows, cols int) (terminalProcess, error) {
	sandbox := b.client.AttachSandbox(sandboxID, envdPort)
	return sandbox.Pty().Create(ctx, cubesandbox.PtySize{Rows: rows, Cols: cols}, cubesandbox.PtyCreateOptions{
		Shell:   "/bin/sh",
		Args:    []string{"-i"},
		Timeout: 24 * time.Hour,
	})
}

func (b *sdkTerminalBackend) Connect(ctx context.Context, sandboxID string, envdPort, pid int) (terminalProcess, error) {
	sandbox := b.client.AttachSandbox(sandboxID, envdPort)
	return sandbox.Pty().Connect(ctx, pid, cubesandbox.PtyConnectOptions{Timeout: 24 * time.Hour})
}

func (b *sdkTerminalBackend) Close() error { return b.client.Close() }

type TerminalSession struct {
	ID          string
	Username    string
	SandboxID   string
	ContainerID string
	EnvdPort    int
	PID         int

	process    terminalProcess
	attachment uint64
}

func (s *TerminalSession) Process() terminalProcess { return s.process }

type terminalSessionState struct {
	session    *TerminalSession
	process    terminalProcess
	attached   bool
	generation uint64
	cleanup    *time.Timer
	createdAt  time.Time
}

// TerminalService owns one-time tickets and resumable PTY session metadata.
// The PTY itself remains inside the sandbox and is always controlled through
// envd, the same mechanism used by the SDK.
type TerminalService struct {
	backend terminalBackend

	ticketTTL      time.Duration
	reconnectGrace time.Duration
	maxSessions    int
	maxPerSandbox  int

	mu       sync.Mutex
	tickets  map[string]TerminalTicket
	sessions map[string]*terminalSessionState
}

func NewTerminalService(proxyURL, sandboxDomain string, ticketTTL, reconnectGrace time.Duration, maxSessions, maxPerSandbox int) (*TerminalService, error) {
	backend, err := newSDKTerminalBackend(proxyURL, sandboxDomain)
	if err != nil {
		return nil, err
	}
	return newTerminalService(backend, ticketTTL, reconnectGrace, maxSessions, maxPerSandbox), nil
}

func newTerminalService(backend terminalBackend, ticketTTL, reconnectGrace time.Duration, maxSessions, maxPerSandbox int) *TerminalService {
	return &TerminalService{
		backend:        backend,
		ticketTTL:      ticketTTL,
		reconnectGrace: reconnectGrace,
		maxSessions:    maxSessions,
		maxPerSandbox:  maxPerSandbox,
		tickets:        make(map[string]TerminalTicket),
		sessions:       make(map[string]*terminalSessionState),
	}
}

func (s *TerminalService) Close() error {
	s.mu.Lock()
	sessions := make([]*TerminalSession, 0, len(s.sessions))
	for _, state := range s.sessions {
		if state.cleanup != nil {
			state.cleanup.Stop()
		}
		session := *state.session
		session.process = state.process
		sessions = append(sessions, &session)
	}
	s.sessions = make(map[string]*terminalSessionState)
	s.tickets = make(map[string]TerminalTicket)
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, session := range sessions {
		if session.process != nil {
			_, _ = session.process.Kill(ctx)
		}
	}
	return s.backend.Close()
}

func (s *TerminalService) IssueTicket(ticket TerminalTicket) (TerminalTicket, error) {
	if ticket.SessionID != "" {
		s.mu.Lock()
		state := s.sessions[ticket.SessionID]
		valid := state != nil && !state.attached &&
			state.session.Username == ticket.Username &&
			state.session.SandboxID == ticket.SandboxID &&
			state.session.ContainerID == ticket.ContainerID
		s.mu.Unlock()
		if !valid {
			return TerminalTicket{}, ErrTerminalSessionGone
		}
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return TerminalTicket{}, fmt.Errorf("create terminal ticket: %w", err)
	}
	ticket.Token = base64.RawURLEncoding.EncodeToString(raw)
	ticket.ExpiresAt = time.Now().Add(s.ticketTTL)

	s.mu.Lock()
	now := time.Now()
	for token, existing := range s.tickets {
		if !existing.ExpiresAt.After(now) {
			delete(s.tickets, token)
		}
	}
	maxTickets := max(1, s.maxSessions*2)
	if len(s.tickets) >= maxTickets {
		s.mu.Unlock()
		return TerminalTicket{}, ErrTerminalSessionLimit
	}
	s.tickets[ticket.Token] = ticket
	s.mu.Unlock()
	return ticket, nil
}

func (s *TerminalService) ConsumeTicket(token, sandboxID string) (TerminalTicket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ticket, ok := s.tickets[token]
	delete(s.tickets, token)
	if !ok || !ticket.ExpiresAt.After(time.Now()) || ticket.SandboxID != sandboxID {
		return TerminalTicket{}, ErrTerminalTicketInvalid
	}
	return ticket, nil
}

func (s *TerminalService) Open(ctx context.Context, ticket TerminalTicket) (*TerminalSession, bool, error) {
	if ticket.SessionID != "" {
		return s.reconnect(ctx, ticket)
	}

	session := &TerminalSession{
		ID:          uuid.NewString(),
		Username:    ticket.Username,
		SandboxID:   ticket.SandboxID,
		ContainerID: ticket.ContainerID,
		EnvdPort:    ticket.EnvdPort,
	}
	state := &terminalSessionState{
		session:    session,
		attached:   true,
		generation: 1,
		createdAt:  time.Now(),
	}

	s.mu.Lock()
	if len(s.sessions) >= s.maxSessions || s.sessionsForSandboxLocked(ticket.SandboxID) >= s.maxPerSandbox {
		s.mu.Unlock()
		return nil, false, ErrTerminalSessionLimit
	}
	s.sessions[session.ID] = state
	s.mu.Unlock()

	process, err := s.backend.Start(ctx, ticket.SandboxID, ticket.EnvdPort, ticket.Rows, ticket.Cols)
	if err != nil {
		s.mu.Lock()
		if s.sessions[session.ID] == state {
			delete(s.sessions, session.ID)
		}
		s.mu.Unlock()
		return nil, false, err
	}

	s.mu.Lock()
	if s.sessions[session.ID] != state || !state.attached || state.generation != 1 {
		s.mu.Unlock()
		closeTerminalProcess(process, true)
		return nil, false, ErrTerminalSessionGone
	}
	state.session.PID = process.PID()
	state.process = process
	attached := *state.session
	attached.process = process
	attached.attachment = state.generation
	s.mu.Unlock()
	return &attached, false, nil
}

func (s *TerminalService) reconnect(ctx context.Context, ticket TerminalTicket) (*TerminalSession, bool, error) {
	s.mu.Lock()
	state := s.sessions[ticket.SessionID]
	if state == nil ||
		state.session.Username != ticket.Username ||
		state.session.SandboxID != ticket.SandboxID ||
		state.session.ContainerID != ticket.ContainerID {
		s.mu.Unlock()
		return nil, false, ErrTerminalSessionGone
	}
	if state.attached {
		s.mu.Unlock()
		return nil, false, ErrTerminalSessionBusy
	}
	state.attached = true
	state.generation++
	generation := state.generation
	if state.cleanup != nil {
		state.cleanup.Stop()
		state.cleanup = nil
	}
	session := *state.session
	s.mu.Unlock()

	process, err := s.backend.Connect(ctx, session.SandboxID, session.EnvdPort, session.PID)
	if err != nil {
		s.mu.Lock()
		if s.sessions[session.ID] == state && state.generation == generation {
			state.attached = false
			s.scheduleCleanupLocked(session.ID, state, generation)
		}
		s.mu.Unlock()
		return nil, false, err
	}

	s.mu.Lock()
	if s.sessions[session.ID] != state || !state.attached || state.generation != generation {
		s.mu.Unlock()
		closeTerminalProcess(process, true)
		return nil, false, ErrTerminalSessionGone
	}
	state.process = process
	session.process = process
	session.attachment = generation
	s.mu.Unlock()
	return &session, true, nil
}

// Detach releases one WebSocket attachment while retaining its PTY during the
// reconnect grace period. The attachment generation prevents a late close
// event from an old WebSocket from detaching a newer, reconnected stream.
func (s *TerminalService) Detach(session *TerminalSession) {
	if session == nil {
		return
	}
	s.mu.Lock()
	state := s.sessions[session.ID]
	if state == nil || !state.attached || state.generation != session.attachment || state.process != session.process {
		s.mu.Unlock()
		return
	}
	state.attached = false
	process := state.process
	s.scheduleCleanupLocked(session.ID, state, state.generation)
	s.mu.Unlock()
	if process != nil {
		_ = process.Disconnect()
	}
}

func (s *TerminalService) scheduleCleanupLocked(sessionID string, state *terminalSessionState, generation uint64) {
	if state.cleanup != nil {
		state.cleanup.Stop()
	}
	state.cleanup = time.AfterFunc(s.reconnectGrace, func() {
		s.cleanupDetached(sessionID, state, generation)
	})
}

func (s *TerminalService) cleanupDetached(sessionID string, expected *terminalSessionState, generation uint64) {
	s.mu.Lock()
	state := s.sessions[sessionID]
	if state != expected || state.attached || state.generation != generation {
		s.mu.Unlock()
		return
	}
	delete(s.sessions, sessionID)
	process := state.process
	s.mu.Unlock()
	closeTerminalProcess(process, true)
}

// Finish removes a session only when the PTY belonging to the current
// attachment has exited. Output closure from a superseded attachment must not
// delete a successfully reconnected session.
func (s *TerminalService) Finish(session *TerminalSession) {
	if session == nil {
		return
	}
	s.mu.Lock()
	state := s.sessions[session.ID]
	if state == nil || state.generation != session.attachment || state.process != session.process {
		s.mu.Unlock()
		return
	}
	if state.cleanup != nil {
		state.cleanup.Stop()
	}
	delete(s.sessions, session.ID)
	s.mu.Unlock()
}

func (s *TerminalService) Terminate(sessionID string) {
	s.mu.Lock()
	state := s.sessions[sessionID]
	if state == nil {
		s.mu.Unlock()
		return
	}
	delete(s.sessions, sessionID)
	if state.cleanup != nil {
		state.cleanup.Stop()
	}
	process := state.process
	s.mu.Unlock()

	closeTerminalProcess(process, true)
}

func closeTerminalProcess(process terminalProcess, kill bool) {
	if process == nil {
		return
	}
	if kill {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = process.Kill(ctx)
		cancel()
	}
	_ = process.Disconnect()
}

func (s *TerminalService) sessionsForSandboxLocked(sandboxID string) int {
	total := 0
	for _, state := range s.sessions {
		if state.session.SandboxID == sandboxID {
			total++
		}
	}
	return total
}
