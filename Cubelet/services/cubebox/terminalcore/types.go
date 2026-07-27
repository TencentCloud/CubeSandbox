// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package terminalcore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

const (
	CodeTargetNotFound    = "TARGET_NOT_FOUND"
	CodeTargetNotRunning  = "TARGET_NOT_RUNNING"
	CodeLimitExceeded     = "LIMIT_EXCEEDED"
	CodeProtocolError     = "PROTOCOL_ERROR"
	CodeShellNotFound     = "SHELL_NOT_FOUND"
	CodeSlowProducer      = "SLOW_PRODUCER"
	CodeSlowConsumer      = "SLOW_CONSUMER"
	CodeSessionLost       = "SESSION_LOST"
	CodeInternal          = "INTERNAL"
	CodeServerDraining    = "SERVER_DRAINING"
	CodeSandboxTransition = "SANDBOX_TRANSITION"
)

const (
	CloseUserClosed        = "USER_CLOSED"
	CloseIdleTimeout       = "IDLE_TIMEOUT"
	CloseMaxLifetime       = "MAX_LIFETIME"
	CloseSandboxTransition = "SANDBOX_TRANSITION"
	CloseRuntimeExited     = "RUNTIME_EXITED"
	CloseSessionLost       = "SESSION_LOST"
	CloseProtocolError     = "PROTOCOL_ERROR"
	CloseServerDraining    = "SERVER_DRAINING"
	CloseSlowConsumer      = "SLOW_CONSUMER"
	CloseInternal          = "INTERNAL"
)

// State is the lifecycle state of one terminal session.
type State string

const (
	StateOpening       State = "opening"
	StateActive        State = "active"
	StateDetachedGrace State = "detached_grace"
	StateClosing       State = "closing"
	StateExited        State = "exited"
	StateClosed        State = "closed"
)

// Config bounds every resource owned by the terminal core. Callers should
// start from DefaultConfig and override only explicitly configured values.
type Config struct {
	MaxFrameBytes        int
	StdinQueueFrames     int
	StdoutChunkBytes     int
	StdoutPendingBytes   int
	ReplayBufferBytes    int
	MaxSessions          int
	MaxSessionsSandbox   int
	MaxSessionsContainer int

	ResizeCoalesce    time.Duration
	OpenTimeout       time.Duration
	ReconnectGrace    time.Duration
	IdleTimeout       time.Duration
	MaxLifetime       time.Duration
	CleanupGrace      time.Duration
	CleanupTimeout    time.Duration
	ReconcileInterval time.Duration
}

func DefaultConfig() Config {
	return Config{
		MaxFrameBytes:        64 << 10,
		StdinQueueFrames:     8,
		StdoutChunkBytes:     32 << 10,
		StdoutPendingBytes:   256 << 10,
		ReplayBufferBytes:    256 << 10,
		MaxSessions:          100,
		MaxSessionsSandbox:   10,
		MaxSessionsContainer: 5,
		ResizeCoalesce:       100 * time.Millisecond,
		OpenTimeout:          10 * time.Second,
		ReconnectGrace:       30 * time.Second,
		IdleTimeout:          30 * time.Minute,
		MaxLifetime:          8 * time.Hour,
		CleanupGrace:         2 * time.Second,
		CleanupTimeout:       10 * time.Second,
		ReconcileInterval:    5 * time.Minute,
	}
}

func (c Config) Validate() error {
	switch {
	case c.MaxFrameBytes <= 0:
		return errors.New("terminal max frame bytes must be positive")
	case c.StdinQueueFrames <= 0:
		return errors.New("terminal stdin queue depth must be positive")
	case c.StdoutChunkBytes <= 0:
		return errors.New("terminal stdout chunk bytes must be positive")
	case c.StdoutPendingBytes < c.StdoutChunkBytes:
		return errors.New("terminal stdout pending bytes must fit one chunk")
	case c.StdoutPendingBytes < c.ReplayBufferBytes:
		return errors.New("terminal stdout pending bytes must fit the replay buffer")
	case c.ReplayBufferBytes <= 0:
		return errors.New("terminal replay buffer bytes must be positive")
	case c.MaxSessions <= 0 || c.MaxSessionsSandbox <= 0 || c.MaxSessionsContainer <= 0:
		return errors.New("terminal session limits must be positive")
	case c.ResizeCoalesce < 0, c.ReconnectGrace < 0:
		return errors.New("terminal resize/reconnect durations cannot be negative")
	case c.OpenTimeout <= 0:
		return errors.New("terminal open timeout must be positive")
	case c.IdleTimeout <= 0, c.MaxLifetime <= 0:
		return errors.New("terminal idle and lifetime durations must be positive")
	case c.CleanupGrace < 0, c.CleanupTimeout <= 0:
		return errors.New("terminal cleanup durations are invalid")
	case c.CleanupGrace >= c.CleanupTimeout:
		return errors.New("terminal cleanup grace must be shorter than cleanup timeout")
	case c.ReconcileInterval <= 0:
		return errors.New("terminal reconcile interval must be positive")
	}
	return nil
}

// TargetMetadata is the durable, non-secret identity needed to recover an
// orphaned containerd exec after cubelet restarts.
type TargetMetadata struct {
	SandboxID          string `json:"sandbox_id"`
	ContainerID        string `json:"container_id"`
	Namespace          string `json:"namespace"`
	RuntimeContainerID string `json:"runtime_container_id"`
}

// Target is resolved entirely inside cubelet. The core never accepts a raw
// runtime handle from its caller.
type Target interface {
	Metadata() TargetMetadata
}

type PTYSpec struct {
	SessionID string
	ExecID    string
	Cols      uint32
	Rows      uint32
}

type ExitStatus struct {
	Code int32
	Err  error
}

// PTYProcess is the narrow runtime surface terminalcore owns. Implementations
// must register Exited before Start returns from RuntimeAdapter.StartPTY.
type PTYProcess interface {
	Stdin() io.WriteCloser
	Stdout() io.Reader
	Resize(context.Context, uint32, uint32) error
	CloseStdin(context.Context) error
	Exited() <-chan ExitStatus
	Kill(context.Context) error
	Delete(context.Context) error
}

type RuntimeAdapter interface {
	Resolve(context.Context, string, string) (Target, error)
	StartPTY(context.Context, Target, PTYSpec) (PTYProcess, error)
	CleanupOrphan(context.Context, JournalRecord) error
}

type JournalRecord struct {
	SessionID string         `json:"session_id"`
	ExecID    string         `json:"exec_id"`
	Target    TargetMetadata `json:"target"`
	OpenedAt  time.Time      `json:"opened_at"`
}

type Journal interface {
	Put(JournalRecord) error
	Remove(string) error
	List() ([]JournalRecord, error)
}

type ResumeRequest struct {
	SessionID  string
	LastOffset uint64
}

type OpenRequest struct {
	RequestID   string
	SandboxID   string
	ContainerID string
	SessionID   string
	Cols        uint32
	Rows        uint32
	Resume      *ResumeRequest
}

type Opened struct {
	SessionID       string
	ReplayFrom      uint64
	ReplayTruncated bool
}

type EventKind uint8

const (
	EventStdout EventKind = iota + 1
	EventExit
	EventError
	EventClose
)

type Event struct {
	Kind     EventKind
	Data     []byte
	Offset   uint64
	ExitCode int32
	Code     string
	Reason   string
}

type CodeError struct {
	Code string
	Err  error
}

func (e *CodeError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err == nil {
		return e.Code
	}
	return fmt.Sprintf("%s: %v", e.Code, e.Err)
}

func (e *CodeError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func Errorf(code, format string, args ...interface{}) error {
	return &CodeError{Code: code, Err: fmt.Errorf(format, args...)}
}

func WrapError(code string, err error) error {
	if err == nil {
		return nil
	}
	return &CodeError{Code: code, Err: err}
}

func CodeOf(err error) string {
	var coded *CodeError
	if errors.As(err, &coded) && coded.Code != "" {
		return coded.Code
	}
	return CodeInternal
}

func sanitizeCloseReason(reason string) string {
	switch reason {
	case CloseUserClosed, CloseIdleTimeout, CloseMaxLifetime,
		CloseSandboxTransition, CloseRuntimeExited, CloseSessionLost,
		CloseProtocolError, CloseServerDraining, CloseSlowConsumer,
		CloseInternal:
		return reason
	default:
		return CloseUserClosed
	}
}
