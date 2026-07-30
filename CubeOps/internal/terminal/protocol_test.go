// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package terminal

import (
	"errors"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func TestValidateClientMessage(t *testing.T) {
	tests := []struct {
		name          string
		messageType   int
		message       []byte
		maxFrameBytes int
		wantStdin     int64
		wantCloseCode int
	}{
		{name: "stdin empty", messageType: websocket.BinaryMessage, message: []byte{ChannelStdin}, maxFrameBytes: 8},
		{name: "stdin at limit", messageType: websocket.BinaryMessage, message: append([]byte{ChannelStdin}, make([]byte, 8)...), maxFrameBytes: 8, wantStdin: 8},
		{name: "stdin over limit", messageType: websocket.BinaryMessage, message: append([]byte{ChannelStdin}, make([]byte, 9)...), maxFrameBytes: 8, wantCloseCode: websocket.CloseMessageTooBig},
		{name: "text", messageType: websocket.TextMessage, message: []byte("input"), maxFrameBytes: 8, wantCloseCode: websocket.ClosePolicyViolation},
		{name: "empty", messageType: websocket.BinaryMessage, maxFrameBytes: 8, wantCloseCode: websocket.ClosePolicyViolation},
		{name: "unknown channel", messageType: websocket.BinaryMessage, message: []byte{0xff}, maxFrameBytes: 8, wantCloseCode: websocket.ClosePolicyViolation},
		{name: "valid resize minimum", messageType: websocket.BinaryMessage, message: resizeMessage(`{"cols":1,"rows":1}`), maxFrameBytes: 64},
		{name: "valid resize maximum", messageType: websocket.BinaryMessage, message: resizeMessage(`{"cols":1000,"rows":1000}`), maxFrameBytes: 64},
		{name: "resize zero", messageType: websocket.BinaryMessage, message: resizeMessage(`{"cols":0,"rows":1}`), maxFrameBytes: 64, wantCloseCode: websocket.ClosePolicyViolation},
		{name: "resize too large", messageType: websocket.BinaryMessage, message: resizeMessage(`{"cols":1001,"rows":1}`), maxFrameBytes: 64, wantCloseCode: websocket.ClosePolicyViolation},
		{name: "resize unknown field", messageType: websocket.BinaryMessage, message: resizeMessage(`{"cols":80,"rows":24,"extra":1}`), maxFrameBytes: 64, wantCloseCode: websocket.ClosePolicyViolation},
		{name: "resize multiple values", messageType: websocket.BinaryMessage, message: resizeMessage(`{"cols":80,"rows":24} {}`), maxFrameBytes: 64, wantCloseCode: websocket.ClosePolicyViolation},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			info, err := ValidateClientMessage(test.messageType, test.message, test.maxFrameBytes)
			if test.wantCloseCode == 0 {
				if err != nil {
					t.Fatalf("ValidateClientMessage: %v", err)
				}
				if info.StdinBytes != test.wantStdin {
					t.Fatalf("StdinBytes = %d, want %d", info.StdinBytes, test.wantStdin)
				}
				return
			}
			var protocolErr *ProtocolError
			if !errors.As(err, &protocolErr) {
				t.Fatalf("error = %T %v, want *ProtocolError", err, err)
			}
			if protocolErr.CloseCode != test.wantCloseCode {
				t.Fatalf("CloseCode = %d, want %d", protocolErr.CloseCode, test.wantCloseCode)
			}
		})
	}
}

func TestValidateServerMessage(t *testing.T) {
	tests := []struct {
		name          string
		messageType   int
		message       []byte
		maxFrameBytes int
		wantType      string
		wantSessionID string
		wantStdout    int64
		wantExitCode  *int32
		wantErrorCode string
		wantReason    string
		wantError     bool
	}{
		{name: "stdout at limit", messageType: websocket.BinaryMessage, message: append([]byte{ChannelStdout}, make([]byte, 8)...), maxFrameBytes: 8, wantType: "stdout", wantStdout: 8},
		{name: "stdout over limit", messageType: websocket.BinaryMessage, message: append([]byte{ChannelStdout}, make([]byte, 9)...), maxFrameBytes: 8, wantError: true},
		{name: "opened", messageType: websocket.BinaryMessage, message: statusMessage(`{"type":"opened","sessionId":"session-a","replay":{"from":7,"truncated":true}}`), maxFrameBytes: 64, wantType: "opened", wantSessionID: "session-a"},
		{name: "exit", messageType: websocket.BinaryMessage, message: statusMessage(`{"type":"exit","exitCode":17}`), maxFrameBytes: 64, wantType: "exit", wantExitCode: int32Pointer(17)},
		{name: "error", messageType: websocket.BinaryMessage, message: statusMessage(`{"type":"error","code":"SLOW_PRODUCER"}`), maxFrameBytes: 64, wantType: "error", wantErrorCode: "SLOW_PRODUCER"},
		{name: "close", messageType: websocket.BinaryMessage, message: statusMessage(`{"type":"close","reason":"RUNTIME_EXITED"}`), maxFrameBytes: 64, wantType: "close", wantReason: "RUNTIME_EXITED"},
		{name: "text", messageType: websocket.TextMessage, message: []byte("invalid"), maxFrameBytes: 64, wantError: true},
		{name: "unknown channel", messageType: websocket.BinaryMessage, message: []byte{ChannelStderr}, maxFrameBytes: 64, wantError: true},
		{name: "opened missing replay", messageType: websocket.BinaryMessage, message: statusMessage(`{"type":"opened","sessionId":"session-a"}`), maxFrameBytes: 64, wantError: true},
		{name: "unknown error code", messageType: websocket.BinaryMessage, message: statusMessage(`{"type":"error","code":"details: secret"}`), maxFrameBytes: 64, wantError: true},
		{name: "unknown close reason", messageType: websocket.BinaryMessage, message: statusMessage(`{"type":"close","reason":"arbitrary"}`), maxFrameBytes: 64, wantError: true},
		{name: "unknown status field", messageType: websocket.BinaryMessage, message: statusMessage(`{"type":"exit","exitCode":0,"detail":"no"}`), maxFrameBytes: 64, wantError: true},
		{name: "multiple status values", messageType: websocket.BinaryMessage, message: statusMessage(`{"type":"exit","exitCode":0} {}`), maxFrameBytes: 64, wantError: true},
		{name: "status over limit", messageType: websocket.BinaryMessage, message: statusMessage(`{"type":"error","code":"` + strings.Repeat("A", maxStatusBytes) + `"}`), maxFrameBytes: 64, wantError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			info, err := ValidateServerMessage(test.messageType, test.message, test.maxFrameBytes)
			if test.wantError {
				if err == nil {
					t.Fatalf("ValidateServerMessage = %+v, nil; want error", info)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateServerMessage: %v", err)
			}
			if info.Type != test.wantType || info.SessionID != test.wantSessionID || info.StdoutBytes != test.wantStdout ||
				info.ErrorCode != test.wantErrorCode || info.CloseReason != test.wantReason {
				t.Fatalf("info = %+v", info)
			}
			if test.wantExitCode != nil && (info.ExitCode == nil || *info.ExitCode != *test.wantExitCode) {
				t.Fatalf("ExitCode = %v, want %d", info.ExitCode, *test.wantExitCode)
			}
		})
	}
}

func TestEncodeGatewayStatusUsesTerminalChannel(t *testing.T) {
	for name, encode := range map[string]func() ([]byte, error){
		"error": func() ([]byte, error) { return EncodeErrorStatus("INTERNAL") },
		"close": func() ([]byte, error) { return EncodeCloseStatus("SERVER_DRAINING") },
	} {
		t.Run(name, func(t *testing.T) {
			message, err := encode()
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if len(message) < 2 || message[0] != ChannelStatus {
				t.Fatalf("message = %q", message)
			}
			if _, err := ValidateServerMessage(websocket.BinaryMessage, message, 64<<10); err != nil {
				t.Fatalf("encoded status did not validate: %v", err)
			}
		})
	}
}

func resizeMessage(payload string) []byte {
	return append([]byte{ChannelResize}, []byte(payload)...)
}

func statusMessage(payload string) []byte {
	return append([]byte{ChannelStatus}, []byte(payload)...)
}

func int32Pointer(value int32) *int32 { return &value }
