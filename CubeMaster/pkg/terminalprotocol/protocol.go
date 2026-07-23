// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package terminalprotocol defines the small, versioned WebSocket control
// protocol shared by CubeOps and CubeMaster. TTY bytes never pass through JSON.
package terminalprotocol

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	Version = 1
	// Keep these wire-level bounds aligned with CubeOps terminal grants and
	// Cubelet terminalcore. The components are separate Go modules, so they
	// cannot import one implementation without coupling their dependency graph.
	MinCols = 2
	MaxCols = 500
	MinRows = 1
	MaxRows = 200
)

type State uint8

const (
	StateAwaitingOpen State = iota
	StateReady
)

type ControlType string

const (
	TypeOpen      ControlType = "open"
	TypeResize    ControlType = "resize"
	TypeKeepalive ControlType = "keepalive"
	TypeClose     ControlType = "close"
	TypeReady     ControlType = "ready"
	TypeError     ControlType = "error"
	TypeExit      ControlType = "exit"
)

type ErrorCode string

const (
	CodeInvalidRequest   ErrorCode = "INVALID_REQUEST"
	CodeTargetNotFound   ErrorCode = "TARGET_NOT_FOUND"
	CodeTargetNotRunning ErrorCode = "TARGET_NOT_RUNNING"
	CodeInternal         ErrorCode = "INTERNAL"
	CodeIdleTimeout      ErrorCode = "IDLE_TIMEOUT"
)

type ClientControl struct {
	Version     int         `json:"v"`
	Type        ControlType `json:"type"`
	RequestID   string      `json:"requestId,omitempty"`
	SessionID   string      `json:"sessionId,omitempty"`
	SandboxID   string      `json:"sandboxId,omitempty"`
	ContainerID string      `json:"containerId,omitempty"`
	Args        []string    `json:"args,omitempty"`
	Env         []string    `json:"env,omitempty"`
	Cwd         string      `json:"cwd,omitempty"`
	Cols        uint32      `json:"cols,omitempty"`
	Rows        uint32      `json:"rows,omitempty"`
}

type ServerControl struct {
	Version   int         `json:"v"`
	Type      ControlType `json:"type"`
	SessionID string      `json:"sessionId,omitempty"`
	Code      ErrorCode   `json:"code,omitempty"`
	Message   string      `json:"message,omitempty"`
	Retryable bool        `json:"retryable,omitempty"`
	ExitCode  int32       `json:"exitCode,omitempty"`
	Reason    string      `json:"reason,omitempty"`
}

func DecodeClientControl(raw []byte, state State) (ClientControl, error) {
	var control ClientControl
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&control); err != nil {
		return control, fmt.Errorf("invalid terminal control: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return control, err
	}
	if control.Version != Version {
		return control, fmt.Errorf("unsupported terminal protocol version %d", control.Version)
	}

	switch state {
	case StateAwaitingOpen:
		if control.Type != TypeOpen {
			return control, errors.New("the first terminal control must be open")
		}
		if strings.TrimSpace(control.RequestID) == "" || strings.TrimSpace(control.SessionID) == "" {
			return control, errors.New("open requires requestId and sessionId")
		}
		if strings.TrimSpace(control.SandboxID) == "" || strings.TrimSpace(control.ContainerID) == "" {
			return control, errors.New("open requires sandboxId and containerId")
		}
		if err := ValidateSize(control.Cols, control.Rows); err != nil {
			return control, err
		}
	case StateReady:
		switch control.Type {
		case TypeResize:
			if err := ValidateSize(control.Cols, control.Rows); err != nil {
				return control, err
			}
		case TypeKeepalive, TypeClose:
		case TypeOpen:
			return control, errors.New("terminal target is already bound")
		default:
			return control, fmt.Errorf("unsupported terminal control type %q", control.Type)
		}
	default:
		return control, errors.New("invalid terminal protocol state")
	}
	return control, nil
}

func ValidateSize(cols, rows uint32) error {
	if cols < MinCols || cols > MaxCols || rows < MinRows || rows > MaxRows {
		return fmt.Errorf("terminal size must be between %dx%d and %dx%d", MinCols, MinRows, MaxCols, MaxRows)
	}
	return nil
}

func EncodeServerControl(control ServerControl) ([]byte, error) {
	if control.Version != Version {
		return nil, fmt.Errorf("unsupported terminal protocol version %d", control.Version)
	}
	switch control.Type {
	case TypeReady, TypeError, TypeExit:
	default:
		return nil, fmt.Errorf("unsupported server control type %q", control.Type)
	}
	return json.Marshal(control)
}

func GatewayTokenMatches(expected, provided string) bool {
	if expected == "" || provided == "" || len(expected) != len(provided) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(provided)) == 1
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("terminal control contains trailing JSON")
	}
	return fmt.Errorf("invalid terminal control: %w", err)
}
