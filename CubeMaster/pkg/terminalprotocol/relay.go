// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package terminalprotocol

import (
	"context"
	"errors"
	"fmt"
	"io"

	cubebox "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
)

const (
	TextMessage   = 1
	BinaryMessage = 2
)

var ErrProtocol = errors.New("terminal protocol error")

type Socket interface {
	ReadMessage() (messageType int, data []byte, err error)
	WriteMessage(messageType int, data []byte) error
	Close() error
}

type Backend interface {
	Send(*cubebox.TerminalClientMessage) error
	Recv() (*cubebox.TerminalServerMessage, error)
	CloseSend() error
}

type BackendFactory interface {
	Open(context.Context, ClientControl) (Backend, error)
}

func Relay(ctx context.Context, socket Socket, factory BackendFactory) error {
	defer socket.Close()
	relayCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	messageType, data, err := socket.ReadMessage()
	if err != nil {
		return err
	}
	if messageType != TextMessage {
		return fmt.Errorf("%w: the first frame must be a text open control", ErrProtocol)
	}
	open, err := DecodeClientControl(data, StateAwaitingOpen)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrProtocol, err)
	}
	backend, err := factory.Open(relayCtx, open)
	if err != nil {
		return err
	}
	defer backend.CloseSend()

	if err := backend.Send(&cubebox.TerminalClientMessage{
		Payload: &cubebox.TerminalClientMessage_Open{Open: &cubebox.TerminalOpenRequest{
			RequestId:   open.RequestID,
			SessionId:   open.SessionID,
			SandboxId:   open.SandboxID,
			ContainerId: open.ContainerID,
			Args:        append([]string(nil), open.Args...),
			Env:         append([]string(nil), open.Env...),
			Cwd:         open.Cwd,
			Cols:        open.Cols,
			Rows:        open.Rows,
		}},
	}); err != nil {
		return err
	}

	clientDone := make(chan error, 1)
	go func() { clientDone <- forwardClient(socket, backend) }()
	serverDone := make(chan error, 1)
	go func() { serverDone <- forwardServer(socket, backend, open.SessionID) }()

	select {
	case err := <-clientDone:
		return normalizeRelayEnd(err)
	case err := <-serverDone:
		return normalizeRelayEnd(err)
	case <-relayCtx.Done():
		return nil
	}
}

func forwardClient(socket Socket, backend Backend) error {
	for {
		messageType, data, err := socket.ReadMessage()
		if err != nil {
			return err
		}
		switch messageType {
		case BinaryMessage:
			if err := backend.Send(&cubebox.TerminalClientMessage{
				Payload: &cubebox.TerminalClientMessage_Stdin{Stdin: append([]byte(nil), data...)},
			}); err != nil {
				return err
			}
		case TextMessage:
			control, err := DecodeClientControl(data, StateReady)
			if err != nil {
				return fmt.Errorf("%w: %v", ErrProtocol, err)
			}
			switch control.Type {
			case TypeResize:
				err = backend.Send(&cubebox.TerminalClientMessage{
					Payload: &cubebox.TerminalClientMessage_Resize{Resize: &cubebox.TerminalResize{
						Cols: control.Cols,
						Rows: control.Rows,
					}},
				})
			case TypeKeepalive:
				continue
			case TypeClose:
				err = backend.Send(&cubebox.TerminalClientMessage{
					Payload: &cubebox.TerminalClientMessage_Close{Close: &cubebox.TerminalClose{}},
				})
				if err == nil {
					return nil
				}
			}
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: unsupported WebSocket message type %d", ErrProtocol, messageType)
		}
	}
}

func forwardServer(socket Socket, backend Backend, sessionID string) error {
	for {
		frame, err := backend.Recv()
		if err != nil {
			return err
		}
		switch payload := frame.GetPayload().(type) {
		case *cubebox.TerminalServerMessage_Ready:
			if payload.Ready == nil {
				return fmt.Errorf("%w: ready payload is missing", ErrProtocol)
			}
			if err := writeServerControl(socket, ServerControl{Version: Version, Type: TypeReady, SessionID: sessionID}); err != nil {
				return err
			}
		case *cubebox.TerminalServerMessage_Output:
			if err := socket.WriteMessage(BinaryMessage, payload.Output); err != nil {
				return err
			}
		case *cubebox.TerminalServerMessage_Exit:
			if payload.Exit == nil {
				return fmt.Errorf("%w: exit payload is missing", ErrProtocol)
			}
			return writeServerControl(socket, ServerControl{
				Version:  Version,
				Type:     TypeExit,
				ExitCode: payload.Exit.GetExitCode(),
				Reason:   payload.Exit.GetReason(),
			})
		case *cubebox.TerminalServerMessage_Error:
			if payload.Error == nil {
				return fmt.Errorf("%w: error payload is missing", ErrProtocol)
			}
			code := grpcErrorCode(payload.Error.GetCode())
			return writeServerControl(socket, ServerControl{
				Version:   Version,
				Type:      TypeError,
				Code:      code,
				Message:   PublicErrorMessage(code),
				Retryable: payload.Error.GetRetryable(),
			})
		default:
			return fmt.Errorf("%w: unsupported backend terminal frame", ErrProtocol)
		}
	}
}

func PublicErrorMessage(code ErrorCode) string {
	switch code {
	case CodeInvalidRequest:
		return "invalid terminal request"
	case CodeTargetNotFound:
		return "terminal target not found"
	case CodeTargetNotRunning:
		return "terminal target is not running"
	case CodeIdleTimeout:
		return "terminal session timed out"
	default:
		return "terminal session failed"
	}
}

func writeServerControl(socket Socket, control ServerControl) error {
	data, err := EncodeServerControl(control)
	if err != nil {
		return err
	}
	return socket.WriteMessage(TextMessage, data)
}

func grpcErrorCode(code cubebox.TerminalErrorCode) ErrorCode {
	switch code {
	case cubebox.TerminalErrorCode_TERMINAL_ERROR_INVALID_REQUEST:
		return CodeInvalidRequest
	case cubebox.TerminalErrorCode_TERMINAL_ERROR_TARGET_NOT_FOUND:
		return CodeTargetNotFound
	case cubebox.TerminalErrorCode_TERMINAL_ERROR_TARGET_NOT_RUNNING:
		return CodeTargetNotRunning
	case cubebox.TerminalErrorCode_TERMINAL_ERROR_IDLE_TIMEOUT:
		return CodeIdleTimeout
	default:
		return CodeInternal
	}
}

func normalizeRelayEnd(err error) error {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
