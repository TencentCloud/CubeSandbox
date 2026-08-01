// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package terminal implements the browser-facing cube-terminal.v1 protocol
// and the stateless CubeOps WebSocket gateway.
package terminal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/gorilla/websocket"
)

const (
	Subprotocol = "cube-terminal.v1"
	GrantPrefix = "cube-grant."

	ChannelStdin  byte = 0x00
	ChannelStdout byte = 0x01
	ChannelStderr byte = 0x02
	ChannelStatus byte = 0x03
	ChannelResize byte = 0x04

	maxStatusBytes = 4 << 10
)

var terminalErrorCodes = map[string]struct{}{
	"TARGET_NOT_FOUND":   {},
	"TARGET_NOT_RUNNING": {},
	"LIMIT_EXCEEDED":     {},
	"PROTOCOL_ERROR":     {},
	"SHELL_NOT_FOUND":    {},
	"SLOW_PRODUCER":      {},
	"SLOW_CONSUMER":      {},
	"SESSION_LOST":       {},
	"INTERNAL":           {},
	"SERVER_DRAINING":    {},
	"SANDBOX_TRANSITION": {},
}

var terminalCloseReasons = map[string]struct{}{
	"USER_CLOSED":        {},
	"IDLE_TIMEOUT":       {},
	"MAX_LIFETIME":       {},
	"SANDBOX_TRANSITION": {},
	"RUNTIME_EXITED":     {},
	"SESSION_LOST":       {},
	"PROTOCOL_ERROR":     {},
	"SERVER_DRAINING":    {},
	"SLOW_PRODUCER":      {},
	"SLOW_CONSUMER":      {},
	"INTERNAL":           {},
}

type ProtocolError struct {
	CloseCode int
	Message   string
}

func (e *ProtocolError) Error() string { return e.Message }

type ClientFrameInfo struct {
	StdinBytes int64
}

type ServerFrameInfo struct {
	Type        string
	SessionID   string
	StdoutBytes int64
	ExitCode    *int32
	ErrorCode   string
	CloseReason string
}

type resizePayload struct {
	Cols uint32 `json:"cols"`
	Rows uint32 `json:"rows"`
}

type replayPayload struct {
	From      uint64 `json:"from"`
	Truncated bool   `json:"truncated"`
}

type statusPayload struct {
	Type      string         `json:"type"`
	SessionID string         `json:"sessionId,omitempty"`
	Replay    *replayPayload `json:"replay,omitempty"`
	ExitCode  *int32         `json:"exitCode,omitempty"`
	Code      string         `json:"code,omitempty"`
	Reason    string         `json:"reason,omitempty"`
}

func ValidateClientMessage(messageType int, message []byte, maxFrameBytes int) (ClientFrameInfo, error) {
	if messageType != websocket.BinaryMessage {
		return ClientFrameInfo{}, &ProtocolError{CloseCode: websocket.ClosePolicyViolation, Message: "binary terminal frames required"}
	}
	if maxFrameBytes <= 0 || len(message) == 0 {
		return ClientFrameInfo{}, &ProtocolError{CloseCode: websocket.ClosePolicyViolation, Message: "terminal frame is empty"}
	}
	if len(message)-1 > maxFrameBytes {
		return ClientFrameInfo{}, &ProtocolError{CloseCode: websocket.CloseMessageTooBig, Message: "terminal frame exceeds limit"}
	}

	payload := message[1:]
	switch message[0] {
	case ChannelStdin:
		return ClientFrameInfo{StdinBytes: int64(len(payload))}, nil
	case ChannelResize:
		if len(payload) == 0 || len(payload) > maxStatusBytes {
			return ClientFrameInfo{}, &ProtocolError{CloseCode: websocket.ClosePolicyViolation, Message: "terminal resize payload is invalid"}
		}
		var resize resizePayload
		if err := decodeStrictJSON(payload, &resize); err != nil {
			return ClientFrameInfo{}, &ProtocolError{CloseCode: websocket.ClosePolicyViolation, Message: "terminal resize payload is invalid"}
		}
		if resize.Cols == 0 || resize.Rows == 0 || resize.Cols > 1000 || resize.Rows > 1000 {
			return ClientFrameInfo{}, &ProtocolError{CloseCode: websocket.ClosePolicyViolation, Message: "terminal dimensions are out of range"}
		}
		return ClientFrameInfo{}, nil
	default:
		return ClientFrameInfo{}, &ProtocolError{
			CloseCode: websocket.ClosePolicyViolation,
			Message:   fmt.Sprintf("terminal channel 0x%02x is not valid from the client", message[0]),
		}
	}
}

func ValidateServerMessage(messageType int, message []byte, maxFrameBytes int) (ServerFrameInfo, error) {
	if messageType != websocket.BinaryMessage {
		return ServerFrameInfo{}, errors.New("terminal relay returned a non-binary frame")
	}
	if maxFrameBytes <= 0 || len(message) == 0 {
		return ServerFrameInfo{}, errors.New("terminal relay returned an empty frame")
	}
	payload := message[1:]
	switch message[0] {
	case ChannelStdout:
		if len(payload) > maxFrameBytes {
			return ServerFrameInfo{}, errors.New("terminal stdout frame exceeds limit")
		}
		return ServerFrameInfo{Type: "stdout", StdoutBytes: int64(len(payload))}, nil
	case ChannelStatus:
		if len(payload) == 0 || len(payload) > maxStatusBytes {
			return ServerFrameInfo{}, errors.New("terminal status frame is invalid")
		}
		var status statusPayload
		if err := decodeStrictJSON(payload, &status); err != nil {
			return ServerFrameInfo{}, fmt.Errorf("decode terminal status: %w", err)
		}
		return validateStatus(status)
	default:
		return ServerFrameInfo{}, fmt.Errorf("terminal relay returned invalid channel 0x%02x", message[0])
	}
}

func validateStatus(status statusPayload) (ServerFrameInfo, error) {
	info := ServerFrameInfo{Type: status.Type}
	switch status.Type {
	case "opened":
		if status.SessionID == "" || status.Replay == nil || status.ExitCode != nil || status.Code != "" || status.Reason != "" {
			return ServerFrameInfo{}, errors.New("terminal opened status schema is invalid")
		}
		info.SessionID = status.SessionID
	case "exit":
		if status.ExitCode == nil || status.SessionID != "" || status.Replay != nil || status.Code != "" || status.Reason != "" {
			return ServerFrameInfo{}, errors.New("terminal exit status schema is invalid")
		}
		info.ExitCode = status.ExitCode
	case "error":
		if status.Code == "" || status.SessionID != "" || status.Replay != nil || status.ExitCode != nil || status.Reason != "" {
			return ServerFrameInfo{}, errors.New("terminal error status schema is invalid")
		}
		if _, ok := terminalErrorCodes[status.Code]; !ok {
			return ServerFrameInfo{}, errors.New("terminal error status code is unknown")
		}
		info.ErrorCode = status.Code
	case "close":
		if status.Reason == "" || status.SessionID != "" || status.Replay != nil || status.ExitCode != nil || status.Code != "" {
			return ServerFrameInfo{}, errors.New("terminal close status schema is invalid")
		}
		if _, ok := terminalCloseReasons[status.Reason]; !ok {
			return ServerFrameInfo{}, errors.New("terminal close status reason is unknown")
		}
		info.CloseReason = status.Reason
	default:
		return ServerFrameInfo{}, errors.New("terminal status type is unknown")
	}
	return info, nil
}

func EncodeErrorStatus(code string) ([]byte, error) {
	if code == "" {
		return nil, errors.New("terminal error code is empty")
	}
	return encodeStatus(statusPayload{Type: "error", Code: code})
}

func EncodeCloseStatus(reason string) ([]byte, error) {
	if reason == "" {
		return nil, errors.New("terminal close reason is empty")
	}
	return encodeStatus(statusPayload{Type: "close", Reason: reason})
}

func encodeStatus(status statusPayload) ([]byte, error) {
	payload, err := json.Marshal(status)
	if err != nil {
		return nil, err
	}
	if len(payload) > maxStatusBytes {
		return nil, errors.New("terminal status exceeds limit")
	}
	message := make([]byte, 1+len(payload))
	message[0] = ChannelStatus
	copy(message[1:], payload)
	return message, nil
}

func decodeStrictJSON(payload []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}
