// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package terminalcore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

type Option func(*Core)

func WithErrorReporter(reporter func(error)) Option {
	return func(c *Core) {
		c.reportError = reporter
	}
}

// Core owns every live PTY on this cubelet. It deliberately has no knowledge
// of gRPC or WebSocket transports.
type Core struct {
	config  Config
	adapter RuntimeAdapter
	journal Journal

	ctx    context.Context
	cancel context.CancelFunc

	mu        sync.Mutex
	sessions  map[string]*session
	draining  map[string]struct{}
	closed    bool
	recoverMu sync.Mutex

	startOnce   sync.Once
	closeOnce   sync.Once
	reportError func(error)
}

func New(parent context.Context, config Config, adapter RuntimeAdapter, journal Journal, opts ...Option) (*Core, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if adapter == nil {
		return nil, errors.New("terminal runtime adapter is nil")
	}
	if journal == nil {
		return nil, errors.New("terminal journal is nil")
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	c := &Core{
		config:   config,
		adapter:  adapter,
		journal:  journal,
		ctx:      ctx,
		cancel:   cancel,
		sessions: make(map[string]*session),
		draining: make(map[string]struct{}),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// Start performs synchronous orphan recovery before starting background
// reconciliation. Valid records are recovered even when another record is
// corrupt; the joined error is returned for operator visibility.
func (c *Core) Start() error {
	var startErr error
	c.startOnce.Do(func() {
		startErr = c.RecoverOrphans(c.ctx)
		go c.run()
	})
	return startErr
}

func (c *Core) run() {
	ticker := time.NewTicker(c.config.ReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			ctx, cancel := context.WithTimeout(context.Background(), c.config.CleanupTimeout)
			if err := c.Close(ctx, CloseServerDraining); err != nil {
				c.report(err)
			}
			cancel()
			return
		case <-ticker.C:
			if err := c.RecoverOrphans(context.Background()); err != nil {
				c.report(err)
			}
		}
	}
}

func (c *Core) Open(ctx context.Context, request OpenRequest) (*Attachment, Opened, error) {
	if err := validateOpenRequest(request); err != nil {
		return nil, Opened{}, err
	}
	if request.Resume != nil {
		return c.resume(request)
	}

	resolveCtx, resolveCancel := context.WithTimeout(ctx, c.config.OpenTimeout)
	target, err := c.adapter.Resolve(resolveCtx, request.SandboxID, request.ContainerID)
	resolveCancel()
	if err != nil {
		return nil, Opened{}, err
	}
	metadata := target.Metadata()
	if metadata.SandboxID == "" || metadata.ContainerID == "" || metadata.RuntimeContainerID == "" {
		return nil, Opened{}, Errorf(CodeInternal, "runtime adapter returned incomplete target metadata")
	}

	s := newSession(c, request, target)
	if err := c.reserve(s); err != nil {
		s.cancel()
		return nil, Opened{}, err
	}

	record := JournalRecord{
		SessionID: request.SessionID,
		ExecID:    s.execID,
		Target:    metadata,
		OpenedAt:  s.openedAt,
	}
	if err := c.journal.Put(record); err != nil {
		s.abortOpen(WrapError(CodeInternal, err))
		return nil, Opened{}, WrapError(CodeInternal, err)
	}
	s.markJournaled()

	openCtx, openCancel := context.WithTimeout(s.ctx, c.config.OpenTimeout)
	process, err := c.adapter.StartPTY(openCtx, target, PTYSpec{
		SessionID: request.SessionID,
		ExecID:    record.ExecID,
		Cols:      request.Cols,
		Rows:      request.Rows,
	})
	openCancel()
	if err != nil {
		s.abortOpen(err)
		return nil, Opened{}, err
	}
	if process == nil {
		err = Errorf(CodeInternal, "runtime adapter returned nil terminal process")
		s.abortOpen(err)
		return nil, Opened{}, err
	}

	attachment, opened, err := s.activate(process)
	if err != nil {
		return nil, Opened{}, err
	}
	return attachment, opened, nil
}

func (c *Core) resume(request OpenRequest) (*Attachment, Opened, error) {
	if request.Resume.SessionID != request.SessionID {
		return nil, Opened{}, Errorf(CodeProtocolError, "resume session id does not match open session id")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.ctx.Err() != nil {
		return nil, Opened{}, Errorf(CodeServerDraining, "terminal core is draining")
	}
	s := c.sessions[request.SessionID]
	if s == nil {
		return nil, Opened{}, Errorf(CodeSessionLost, "terminal session is not available")
	}
	if _, blocked := c.draining[s.target.Metadata().SandboxID]; blocked {
		return nil, Opened{}, Errorf(CodeSandboxTransition, "sandbox is transitioning")
	}
	// Keep the admission fence check and the state transition under c.mu. This
	// serializes resume with DrainSandbox: a resume either wins before the
	// fence is installed or is rejected after it, never reactivating a session
	// after drain has started.
	return s.resume(request)
}

func (c *Core) reserve(s *session) error {
	metadata := s.target.Metadata()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.ctx.Err() != nil {
		return Errorf(CodeServerDraining, "terminal core is draining")
	}
	if _, blocked := c.draining[metadata.SandboxID]; blocked {
		return Errorf(CodeSandboxTransition, "sandbox is transitioning")
	}
	if _, exists := c.sessions[s.id]; exists {
		return Errorf(CodeProtocolError, "terminal session id already exists")
	}
	if len(c.sessions) >= c.config.MaxSessions {
		return Errorf(CodeLimitExceeded, "cubelet terminal session limit reached")
	}
	var sandboxCount, containerCount int
	for _, current := range c.sessions {
		currentTarget := current.target.Metadata()
		if currentTarget.SandboxID == metadata.SandboxID {
			sandboxCount++
			if currentTarget.ContainerID == metadata.ContainerID {
				containerCount++
			}
		}
	}
	if sandboxCount >= c.config.MaxSessionsSandbox {
		return Errorf(CodeLimitExceeded, "sandbox terminal session limit reached")
	}
	if containerCount >= c.config.MaxSessionsContainer {
		return Errorf(CodeLimitExceeded, "container terminal session limit reached")
	}
	c.sessions[s.id] = s
	return nil
}

func (c *Core) unregister(s *session) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if current := c.sessions[s.id]; current == s {
		delete(c.sessions, s.id)
	}
}

// DrainSandbox atomically blocks new opens and closes every existing session
// before the caller continues a pause or destroy transition.
func (c *Core) DrainSandbox(ctx context.Context, sandboxID, reason string) error {
	if sandboxID == "" {
		return Errorf(CodeProtocolError, "sandbox id is empty")
	}
	reason = sanitizeCloseReason(reason)
	c.mu.Lock()
	c.draining[sandboxID] = struct{}{}
	sessions := make([]*session, 0)
	for _, s := range c.sessions {
		if s.target.Metadata().SandboxID == sandboxID {
			sessions = append(sessions, s)
		}
	}
	c.mu.Unlock()

	for _, s := range sessions {
		s.requestClose(reason, nil)
	}
	return waitSessions(ctx, sessions)
}

// AllowSandbox removes the admission fence after a successful resume or a
// failed pause that left the sandbox running.
func (c *Core) AllowSandbox(sandboxID string) {
	c.mu.Lock()
	delete(c.draining, sandboxID)
	c.mu.Unlock()
}

func (c *Core) Close(ctx context.Context, reason string) error {
	reason = sanitizeCloseReason(reason)
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		for _, s := range c.sessions {
			c.draining[s.target.Metadata().SandboxID] = struct{}{}
		}
		c.mu.Unlock()
	})

	c.mu.Lock()
	sessions := make([]*session, 0, len(c.sessions))
	for _, s := range c.sessions {
		sessions = append(sessions, s)
	}
	c.mu.Unlock()
	for _, s := range sessions {
		s.requestClose(reason, nil)
	}
	err := waitSessions(ctx, sessions)
	c.cancel()
	return err
}

func (c *Core) RecoverOrphans(ctx context.Context) error {
	c.recoverMu.Lock()
	defer c.recoverMu.Unlock()

	records, listErr := c.journal.List()
	var errs []error
	if listErr != nil {
		errs = append(errs, listErr)
	}
	for _, record := range records {
		c.mu.Lock()
		_, active := c.sessions[record.SessionID]
		c.mu.Unlock()
		if active {
			continue
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.config.CleanupTimeout)
		err := c.adapter.CleanupOrphan(cleanupCtx, record)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("recover terminal session %s: %w", record.SessionID, err))
			continue
		}
		if err := c.journal.Remove(record.SessionID); err != nil {
			errs = append(errs, fmt.Errorf("remove recovered terminal session %s: %w", record.SessionID, err))
		}
	}
	return errors.Join(errs...)
}

func (c *Core) MaxFrameBytes() int { return c.config.MaxFrameBytes }

func (c *Core) SessionState(sessionID string) (State, bool) {
	c.mu.Lock()
	s := c.sessions[sessionID]
	c.mu.Unlock()
	if s == nil {
		return StateClosed, false
	}
	return s.currentState(), true
}

func (c *Core) report(err error) {
	if err != nil && c.reportError != nil {
		c.reportError(err)
	}
}

func waitSessions(ctx context.Context, sessions []*session) error {
	var errs []error
	for _, s := range sessions {
		select {
		case <-s.done:
			if err := s.getCleanupError(); err != nil {
				errs = append(errs, fmt.Errorf("terminal session %s cleanup: %w", s.id, err))
			}
		case <-ctx.Done():
			return errors.Join(append(errs, ctx.Err())...)
		}
	}
	return errors.Join(errs...)
}

func validateOpenRequest(request OpenRequest) error {
	if request.SandboxID == "" {
		return Errorf(CodeProtocolError, "sandbox id is empty")
	}
	if _, err := uuid.Parse(request.SessionID); err != nil {
		return Errorf(CodeProtocolError, "invalid session id")
	}
	if request.Cols < minTerminalDimension || request.Rows < minTerminalDimension || request.Cols > maxTerminalDimension || request.Rows > maxTerminalDimension {
		return Errorf(CodeProtocolError, "terminal dimensions are out of range")
	}
	if len(request.RequestID) > 256 || len(request.SandboxID) > 256 || len(request.ContainerID) > 256 {
		return Errorf(CodeProtocolError, "terminal identifier is too long")
	}
	if request.Resume != nil {
		if _, err := uuid.Parse(request.Resume.SessionID); err != nil {
			return Errorf(CodeProtocolError, "invalid resume session id")
		}
	}
	return nil
}

func execIDForSession(sessionID string) string {
	prefix := sessionID
	if len(prefix) > 12 {
		prefix = prefix[:12]
	}
	return "cubelet-term-" + prefix
}
