// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package terminalcore

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

type inputFrame struct {
	generation uint64
	data       []byte
}

type session struct {
	core     *Core
	id       string
	request  OpenRequest
	target   Target
	execID   string
	openedAt time.Time

	ctx    context.Context
	cancel context.CancelFunc

	mu               sync.Mutex
	state            State
	process          PTYProcess
	generation       uint64
	attachment       *Attachment
	closeReason      string
	exitStatus       *ExitStatus
	journaled        bool
	detachedAt       uint64
	graceEpoch       uint64
	resizeCols       uint32
	resizeRows       uint32
	resizeGeneration uint64
	cleanupErr       error

	ring *ringBuffer

	stdinQueue   chan inputFrame
	resizeNotify chan struct{}
	activity     chan struct{}
	lastActivity atomic.Int64

	openDone     chan struct{}
	openDoneOnce sync.Once
	exitedDone   chan struct{}
	exitedOnce   sync.Once
	stdoutDone   chan struct{}
	stdinDone    chan struct{}
	resizeDone   chan struct{}
	timerDone    chan struct{}
	done         chan struct{}
	cleanupOnce  sync.Once
}

func newSession(core *Core, request OpenRequest, target Target) *session {
	ctx, cancel := context.WithCancel(core.ctx)
	now := time.Now().UTC()
	s := &session{
		core:         core,
		id:           request.SessionID,
		request:      request,
		target:       target,
		execID:       execIDForSession(request.SessionID),
		openedAt:     now,
		ctx:          ctx,
		cancel:       cancel,
		state:        StateOpening,
		ring:         newRingBuffer(core.config.ReplayBufferBytes),
		stdinQueue:   make(chan inputFrame, core.config.StdinQueueFrames),
		resizeNotify: make(chan struct{}, 1),
		activity:     make(chan struct{}, 1),
		openDone:     make(chan struct{}),
		exitedDone:   make(chan struct{}),
		stdoutDone:   make(chan struct{}),
		stdinDone:    make(chan struct{}),
		resizeDone:   make(chan struct{}),
		timerDone:    make(chan struct{}),
		done:         make(chan struct{}),
	}
	s.lastActivity.Store(now.UnixNano())
	return s
}

func (s *session) markJournaled() {
	s.mu.Lock()
	s.journaled = true
	s.mu.Unlock()
}

func (s *session) abortOpen(openErr error) {
	s.mu.Lock()
	if s.closeReason == "" {
		s.closeReason = CloseInternal
	}
	s.mu.Unlock()
	s.signalOpenDone()
	s.requestClose(CloseInternal, nil)
	<-s.done
	if openErr != nil {
		s.core.report(openErr)
	}
}

func (s *session) activate(process PTYProcess) (*Attachment, Opened, error) {
	s.mu.Lock()
	s.process = process
	if s.state == StateClosing || s.state == StateExited || s.state == StateClosed {
		reason := s.closeReason
		s.mu.Unlock()
		s.signalOpenDone()
		<-s.done
		if reason == CloseSandboxTransition {
			return nil, Opened{}, Errorf(CodeSandboxTransition, "sandbox transitioned while terminal was opening")
		}
		return nil, Opened{}, Errorf(CodeSessionLost, "terminal closed while opening")
	}
	s.state = StateActive
	s.generation = 1
	attachment := newAttachment(s, s.generation, Opened{
		SessionID: s.id,
		ExecID:    s.execID,
		Target:    s.target.Metadata(),
	}, s.core.config)
	s.attachment = attachment
	s.mu.Unlock()

	s.startPumps()
	s.signalOpenDone()
	return attachment, attachment.opened, nil
}

func (s *session) resume(request OpenRequest) (*Attachment, Opened, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	metadata := s.target.Metadata()
	if metadata.SandboxID != request.SandboxID {
		return nil, Opened{}, Errorf(CodeSessionLost, "resume target does not match session")
	}
	if request.ContainerID != "" && request.ContainerID != metadata.ContainerID {
		return nil, Opened{}, Errorf(CodeSessionLost, "resume container does not match session")
	}
	if s.state != StateDetachedGrace || s.attachment != nil {
		return nil, Opened{}, Errorf(CodeSessionLost, "terminal session is not detached")
	}

	replay, replayFrom, truncated := s.ring.ReadFrom(request.Resume.LastOffset)
	nextGeneration := s.generation + 1
	opened := Opened{
		SessionID:       s.id,
		ReplayFrom:      replayFrom,
		ReplayTruncated: truncated,
		ExecID:          s.execID,
		Target:          metadata,
	}
	attachment := newAttachment(s, nextGeneration, opened, s.core.config)
	for len(replay) > 0 {
		n := s.core.config.StdoutChunkBytes
		if n > len(replay) {
			n = len(replay)
		}
		chunk := append([]byte(nil), replay[:n]...)
		if !attachment.enqueue(Event{Kind: EventStdout, Data: chunk, Offset: replayFrom}) {
			attachment.finish()
			return nil, Opened{}, Errorf(CodeSlowConsumer, "replay exceeds terminal output queue")
		}
		replay = replay[n:]
		replayFrom += uint64(n)
	}
	s.generation = nextGeneration
	s.graceEpoch++
	s.state = StateActive
	s.attachment = attachment
	s.resizeCols = request.Cols
	s.resizeRows = request.Rows
	s.resizeGeneration = s.generation
	// A resume re-activates the session: reset the idle clock so the detached
	// time spent offline does not count against the reconnected client.
	s.lastActivity.Store(time.Now().UnixNano())
	select {
	case s.resizeNotify <- struct{}{}:
	default:
	}
	select {
	case s.activity <- struct{}{}:
	default:
	}
	return attachment, opened, nil
}

func (s *session) startPumps() {
	go s.pumpStdout()
	go s.pumpStdin()
	go s.pumpResize()
	go s.watchExit()
	go s.watchTimers()
}

func (s *session) pumpStdout() {
	defer close(s.stdoutDone)
	buf := make([]byte, s.core.config.StdoutChunkBytes)
	for {
		n, err := s.process.Stdout().Read(buf)
		if n > 0 {
			s.handleOutput(append([]byte(nil), buf[:n]...))
		}
		if err != nil {
			// ErrClosedPipe is the expected result of cleanup closing the read
			// side after process.Delete; it is not an internal fault.
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) && s.currentState() != StateClosed {
				s.core.report(WrapError(CodeInternal, err))
			}
			return
		}
	}
}

func (s *session) handleOutput(data []byte) {
	var (
		detached         *Attachment
		closeForOverflow bool
	)
	s.mu.Lock()
	offset := s.ring.Write(data)
	// Output counts as activity too: a session streaming output (tail -f,
	// watch, a long build) with no keystrokes is alive and must not be
	// reaped as idle.
	s.lastActivity.Store(time.Now().UnixNano())
	switch s.state {
	case StateActive:
		if s.attachment != nil && !s.attachment.enqueue(Event{Kind: EventStdout, Data: data, Offset: offset}) {
			detached = s.attachment
			s.attachment = nil
			s.state = StateDetachedGrace
			s.detachedAt = s.ring.End()
			s.graceEpoch++
		}
	case StateClosing, StateExited:
		if s.attachment != nil && !s.attachment.enqueue(Event{Kind: EventStdout, Data: data, Offset: offset}) {
			detached = s.attachment
			s.attachment = nil
		}
	case StateDetachedGrace:
		closeForOverflow = s.ring.Start() > s.detachedAt
	}
	epoch := s.graceEpoch
	s.mu.Unlock()

	select {
	case s.activity <- struct{}{}:
	default:
	}

	if detached != nil {
		detached.finish(Event{Kind: EventError, Code: CodeSlowConsumer})
		if s.currentState() == StateDetachedGrace {
			s.startGraceTimer(epoch)
		}
	}
	if closeForOverflow {
		s.requestClose(CloseSessionLost, nil)
	}
}

func (s *session) pumpStdin() {
	defer close(s.stdinDone)
	for {
		select {
		case <-s.ctx.Done():
			return
		case frame := <-s.stdinQueue:
			s.mu.Lock()
			valid := s.state == StateActive && s.generation == frame.generation && s.attachment != nil
			process := s.process
			s.mu.Unlock()
			if !valid || process == nil {
				continue
			}
			if _, err := process.Stdin().Write(frame.data); err != nil {
				if s.ctx.Err() == nil {
					s.core.report(WrapError(CodeInternal, err))
					s.requestClose(CloseRuntimeExited, nil)
				}
				return
			}
		}
	}
}

func (s *session) pumpResize() {
	defer close(s.resizeDone)
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.resizeNotify:
			if s.core.config.ResizeCoalesce > 0 {
				timer := time.NewTimer(s.core.config.ResizeCoalesce)
				select {
				case <-s.ctx.Done():
					if !timer.Stop() {
						<-timer.C
					}
					return
				case <-timer.C:
				}
			}
			s.mu.Lock()
			cols, rows := s.resizeCols, s.resizeRows
			valid := s.state == StateActive && s.resizeGeneration == s.generation && s.attachment != nil
			process := s.process
			s.mu.Unlock()
			if !valid || process == nil {
				continue
			}
			resizeCtx, cancel := context.WithTimeout(s.ctx, s.core.config.CleanupTimeout)
			err := process.Resize(resizeCtx, cols, rows)
			cancel()
			if err != nil && s.ctx.Err() == nil {
				s.core.report(WrapError(CodeInternal, err))
				s.requestClose(CloseInternal, nil)
				return
			}
		}
	}
}

func (s *session) watchExit() {
	status, ok := <-s.process.Exited()
	if !ok {
		status = ExitStatus{Code: 255, Err: errors.New("runtime exit channel closed without status")}
	}
	s.exitedOnce.Do(func() {
		s.mu.Lock()
		copyStatus := status
		s.exitStatus = &copyStatus
		s.mu.Unlock()
		close(s.exitedDone)
	})
	s.requestClose(CloseRuntimeExited, &status)
}

func (s *session) watchTimers() {
	defer close(s.timerDone)
	idleTimer := time.NewTimer(s.core.config.IdleTimeout)
	hardTimer := time.NewTimer(s.core.config.MaxLifetime)
	defer idleTimer.Stop()
	defer hardTimer.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-hardTimer.C:
			s.requestClose(CloseMaxLifetime, nil)
			return
		case <-idleTimer.C:
			last := time.Unix(0, s.lastActivity.Load())
			remaining := s.core.config.IdleTimeout - time.Since(last)
			if remaining > 0 {
				idleTimer.Reset(remaining)
				continue
			}
			s.requestClose(CloseIdleTimeout, nil)
			return
		case <-s.activity:
			last := time.Unix(0, s.lastActivity.Load())
			remaining := s.core.config.IdleTimeout - time.Since(last)
			if remaining <= 0 {
				remaining = time.Nanosecond
			}
			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			idleTimer.Reset(remaining)
		}
	}
}

func (s *session) requestClose(reason string, exitStatus *ExitStatus) {
	reason = sanitizeCloseReason(reason)
	s.mu.Lock()
	if s.state == StateClosed {
		s.mu.Unlock()
		return
	}
	if s.closeReason == "" {
		s.closeReason = reason
	}
	if exitStatus != nil {
		copyStatus := *exitStatus
		s.exitStatus = &copyStatus
		s.state = StateExited
	} else if s.state != StateExited {
		s.state = StateClosing
	}
	s.mu.Unlock()
	s.cancel()
	s.cleanupOnce.Do(func() { go s.cleanup() })
}

func (s *session) cleanup() {
	<-s.openDone
	cleanupCtx, cancel := context.WithTimeout(context.Background(), s.core.config.CleanupTimeout)
	defer cancel()

	s.mu.Lock()
	process := s.process
	journaled := s.journaled
	s.mu.Unlock()

	var errs []error
	processDeleted := process == nil
	if process != nil {
		if err := process.CloseStdin(cleanupCtx); err != nil {
			errs = append(errs, err)
		}
		if !channelClosed(s.exitedDone) {
			graceTimer := time.NewTimer(s.core.config.CleanupGrace)
			select {
			case <-s.exitedDone:
				if !graceTimer.Stop() {
					<-graceTimer.C
				}
			case <-graceTimer.C:
				if err := process.Kill(cleanupCtx); err != nil {
					errs = append(errs, err)
				}
			case <-cleanupCtx.Done():
				if !graceTimer.Stop() {
					select {
					case <-graceTimer.C:
					default:
					}
				}
			}
		}
		if !channelClosed(s.exitedDone) {
			select {
			case <-s.exitedDone:
			case <-cleanupCtx.Done():
				errs = append(errs, errors.New("terminal process did not exit before cleanup deadline"))
			}
		}
		if err := process.Delete(cleanupCtx); err != nil {
			errs = append(errs, err)
		} else {
			processDeleted = true
		}
		select {
		case <-s.stdoutDone:
		case <-cleanupCtx.Done():
			errs = append(errs, errors.New("terminal stdout pump did not exit before cleanup deadline"))
		}
		for _, pumpDone := range []<-chan struct{}{s.stdinDone, s.resizeDone, s.timerDone} {
			select {
			case <-pumpDone:
			case <-cleanupCtx.Done():
				errs = append(errs, errors.New("terminal worker did not exit before cleanup deadline"))
			}
		}
	}

	if journaled && processDeleted {
		if err := s.core.journal.Remove(s.id); err != nil {
			errs = append(errs, err)
		}
	}

	s.core.unregister(s)
	s.mu.Lock()
	s.state = StateClosed
	s.cleanupErr = errors.Join(errs...)
	attachment := s.attachment
	s.attachment = nil
	reason := s.closeReason
	exitStatus := s.exitStatus
	cleanupErr := s.cleanupErr
	s.mu.Unlock()

	if attachment != nil {
		final := make([]Event, 0, 3)
		if cleanupErr != nil {
			final = append(final, Event{Kind: EventError, Code: CodeInternal})
		}
		if exitStatus != nil {
			final = append(final, Event{Kind: EventExit, ExitCode: exitStatus.Code})
		}
		final = append(final, Event{Kind: EventClose, Reason: reason})
		attachment.finish(final...)
	}
	if cleanupErr != nil {
		s.core.report(cleanupErr)
	}
	close(s.done)
}

func (s *session) detach(generation uint64, code string) {
	s.mu.Lock()
	if s.state != StateActive || s.generation != generation || s.attachment == nil {
		s.mu.Unlock()
		return
	}
	attachment := s.attachment
	s.attachment = nil
	s.state = StateDetachedGrace
	s.detachedAt = s.ring.End()
	s.graceEpoch++
	epoch := s.graceEpoch
	s.mu.Unlock()
	if code != "" {
		attachment.finish(Event{Kind: EventError, Code: code})
	} else {
		attachment.finish()
	}
	s.startGraceTimer(epoch)
}

func (s *session) startGraceTimer(epoch uint64) {
	if s.core.config.ReconnectGrace == 0 {
		s.requestClose(CloseSessionLost, nil)
		return
	}
	time.AfterFunc(s.core.config.ReconnectGrace, func() {
		// The epoch check and the state transition must be atomic: a resume
		// that lands between them (in a plain check-then-requestClose) would
		// leave a freshly re-activated session immediately torn down.
		s.mu.Lock()
		expired := s.state == StateDetachedGrace && s.graceEpoch == epoch
		if expired {
			if s.closeReason == "" {
				s.closeReason = CloseSessionLost
			}
			s.state = StateClosing
		}
		s.mu.Unlock()
		if expired {
			s.cancel()
			s.cleanupOnce.Do(func() { go s.cleanup() })
		}
	})
}

func (s *session) signalOpenDone() {
	s.openDoneOnce.Do(func() { close(s.openDone) })
}

func (s *session) currentState() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *session) getCleanupError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cleanupErr
}

func channelClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// Attachment is one transport generation for a session. Old generations are
// fenced from stdin and resize as soon as a resume succeeds.
type Attachment struct {
	session    *session
	generation uint64
	opened     Opened
	events     chan Event

	mu             sync.Mutex
	closed         bool
	pendingBytes   int
	pendingFrames  int
	pendingControl int
	maxBytes       int
	maxFrames      int
	final          []Event
	finalIndex     int
	finishOnce     sync.Once
}

func newAttachment(s *session, generation uint64, opened Opened, config Config) *Attachment {
	maxFrames := config.StdoutPendingBytes / config.StdoutChunkBytes
	if config.StdoutPendingBytes%config.StdoutChunkBytes != 0 {
		maxFrames++
	}
	return &Attachment{
		session:    s,
		generation: generation,
		opened:     opened,
		events:     make(chan Event, maxFrames+5),
		maxBytes:   config.StdoutPendingBytes,
		maxFrames:  maxFrames,
	}
}

func (a *Attachment) Opened() Opened { return a.opened }

func (a *Attachment) Next(ctx context.Context) (Event, error) {
	select {
	case <-ctx.Done():
		return Event{}, ctx.Err()
	case event, ok := <-a.events:
		if !ok {
			a.mu.Lock()
			if a.finalIndex < len(a.final) {
				event = a.final[a.finalIndex]
				a.finalIndex++
				a.mu.Unlock()
				return event, nil
			}
			a.mu.Unlock()
			return Event{}, io.EOF
		}
		a.mu.Lock()
		if event.Kind == EventStdout {
			a.pendingBytes -= len(event.Data)
			a.pendingFrames--
		} else if a.pendingControl > 0 {
			a.pendingControl--
		}
		a.mu.Unlock()
		return event, nil
	}
}

func (a *Attachment) SendStdin(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if len(data) > a.session.core.config.MaxFrameBytes {
		return Errorf(CodeProtocolError, "stdin frame exceeds limit")
	}
	a.session.mu.Lock()
	valid := a.session.state == StateActive && a.session.generation == a.generation && a.session.attachment == a
	a.session.mu.Unlock()
	if !valid {
		return Errorf(CodeSessionLost, "terminal attachment is stale")
	}
	frame := inputFrame{generation: a.generation, data: append([]byte(nil), data...)}
	select {
	case a.session.stdinQueue <- frame:
		now := time.Now()
		a.session.lastActivity.Store(now.UnixNano())
		select {
		case a.session.activity <- struct{}{}:
		default:
		}
		return nil
	default:
		return Errorf(CodeSlowProducer, "terminal stdin queue is full")
	}
}

func (a *Attachment) Resize(cols, rows uint32) error {
	if cols == 0 || rows == 0 || cols > 1000 || rows > 1000 {
		return Errorf(CodeProtocolError, "terminal dimensions are out of range")
	}
	a.session.mu.Lock()
	if a.session.state != StateActive || a.session.generation != a.generation || a.session.attachment != a {
		a.session.mu.Unlock()
		return Errorf(CodeSessionLost, "terminal attachment is stale")
	}
	a.session.resizeCols = cols
	a.session.resizeRows = rows
	a.session.resizeGeneration = a.generation
	a.session.mu.Unlock()
	select {
	case a.session.resizeNotify <- struct{}{}:
	default:
	}
	return nil
}

func (a *Attachment) NotifyError(code string) {
	if code == "" {
		return
	}
	if !a.enqueue(Event{Kind: EventError, Code: code}) {
		a.session.detach(a.generation, CodeSlowConsumer)
	}
}

func (a *Attachment) Close(reason string) error {
	a.session.mu.Lock()
	valid := a.session.generation == a.generation && a.session.attachment == a && a.session.state == StateActive
	a.session.mu.Unlock()
	if !valid {
		return Errorf(CodeSessionLost, "terminal attachment is stale")
	}
	a.session.requestClose(sanitizeCloseReason(reason), nil)
	return nil
}

func (a *Attachment) Detach() {
	a.session.detach(a.generation, "")
}

func (a *Attachment) enqueue(event Event) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return false
	}
	if event.Kind == EventStdout {
		if a.pendingBytes+len(event.Data) > a.maxBytes || a.pendingFrames >= a.maxFrames {
			return false
		}
	}
	// Control events (error notifications) share the channel's control-frame
	// headroom and are never silently accepted-but-dropped: if the channel is
	// genuinely full, enqueue reports false and the caller handles it.
	select {
	case a.events <- event:
		if event.Kind == EventStdout {
			a.pendingBytes += len(event.Data)
			a.pendingFrames++
		} else {
			a.pendingControl++
		}
		return true
	default:
		return false
	}
}

func (a *Attachment) finish(final ...Event) {
	a.finishOnce.Do(func() {
		a.mu.Lock()
		if a.closed {
			a.mu.Unlock()
			return
		}
		a.closed = true
		a.final = append([]Event(nil), final...)
		close(a.events)
		a.mu.Unlock()
	})
}
