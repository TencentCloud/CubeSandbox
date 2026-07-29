// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package terminalprotocol maps the versioned WebSocket channel protocol to
// the typed Cubelet Terminal gRPC stream. It contains no routing or session
// state, so CubeOps and CubeMaster can mirror the same wire contract in tests.
package terminalprotocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	cubebox "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
)

const (
	Subprotocol = "cube-terminal.v1"

	ChannelStdin  byte = 0x00
	ChannelStdout byte = 0x01
	ChannelStderr byte = 0x02 // Reserved: TTY stderr is merged into stdout.
	ChannelStatus byte = 0x03
	ChannelResize byte = 0x04

	maxStatusBytes = 4 << 10
)

type resizePayload struct {
	Cols uint32 `json:"cols"`
	Rows uint32 `json:"rows"`
}

type replayStatus struct {
	From      uint64 `json:"from"`
	Truncated bool   `json:"truncated"`
}

type statusPayload struct {
	Type      string        `json:"type"`
	SessionID string        `json:"sessionId,omitempty"`
	Replay    *replayStatus `json:"replay,omitempty"`
	ExitCode  *int32        `json:"exitCode,omitempty"`
	Code      string        `json:"code,omitempty"`
	Reason    string        `json:"reason,omitempty"`
}

// DecodeClientFrame accepts only client-owned channels. Open is synthesized
// from authenticated HTTP headers, and close is represented by WebSocket close
// semantics, so neither is accepted as an arbitrary binary payload.
func DecodeClientFrame(message []byte, maxFrameBytes int) (*cubebox.TerminalClientFrame, error) {
	if maxFrameBytes <= 0 {
		return nil, errors.New("terminal frame limit is invalid")
	}
	if len(message) == 0 {
		return nil, errors.New("terminal frame is empty")
	}
	if len(message)-1 > maxFrameBytes {
		return nil, errors.New("terminal frame exceeds limit")
	}

	payload := message[1:]
	switch message[0] {
	case ChannelStdin:
		return &cubebox.TerminalClientFrame{
			Frame: &cubebox.TerminalClientFrame_Stdin{Stdin: append([]byte(nil), payload...)},
		}, nil
	case ChannelResize:
		if len(payload) == 0 || len(payload) > maxStatusBytes {
			return nil, errors.New("terminal resize payload is invalid")
		}
		var resize resizePayload
		if err := decodeStrictJSON(payload, &resize); err != nil {
			return nil, fmt.Errorf("decode terminal resize: %w", err)
		}
		if resize.Cols == 0 || resize.Rows == 0 || resize.Cols > 1000 || resize.Rows > 1000 {
			return nil, errors.New("terminal dimensions are out of range")
		}
		return &cubebox.TerminalClientFrame{
			Frame: &cubebox.TerminalClientFrame_Resize{Resize: &cubebox.TerminalResize{
				Cols: resize.Cols,
				Rows: resize.Rows,
			}},
		}, nil
	default:
		return nil, fmt.Errorf("terminal channel 0x%02x is not valid from the client", message[0])
	}
}

func EncodeServerFrame(frame *cubebox.TerminalServerFrame, maxFrameBytes int) ([]byte, error) {
	if frame == nil {
		return nil, errors.New("terminal server frame is nil")
	}
	if maxFrameBytes <= 0 {
		return nil, errors.New("terminal frame limit is invalid")
	}

	switch payload := frame.GetFrame().(type) {
	case *cubebox.TerminalServerFrame_Opened:
		if payload.Opened == nil {
			return nil, errors.New("terminal opened frame is nil")
		}
		return encodeStatus(statusPayload{
			Type:      "opened",
			SessionID: payload.Opened.GetSessionId(),
			Replay: &replayStatus{
				From:      payload.Opened.GetReplayFrom(),
				Truncated: payload.Opened.GetReplayTruncated(),
			},
		})
	case *cubebox.TerminalServerFrame_Stdout:
		if payload.Stdout == nil {
			return nil, errors.New("terminal stdout frame is nil")
		}
		if len(payload.Stdout.GetData()) > maxFrameBytes {
			return nil, errors.New("terminal stdout frame exceeds limit")
		}
		message := make([]byte, 1+len(payload.Stdout.GetData()))
		message[0] = ChannelStdout
		copy(message[1:], payload.Stdout.GetData())
		return message, nil
	case *cubebox.TerminalServerFrame_Exit:
		if payload.Exit == nil {
			return nil, errors.New("terminal exit frame is nil")
		}
		exitCode := payload.Exit.GetExitCode()
		return encodeStatus(statusPayload{Type: "exit", ExitCode: &exitCode})
	case *cubebox.TerminalServerFrame_Error:
		if payload.Error == nil || payload.Error.GetCode() == "" {
			return nil, errors.New("terminal error frame is invalid")
		}
		return encodeStatus(statusPayload{Type: "error", Code: payload.Error.GetCode()})
	case *cubebox.TerminalServerFrame_Close:
		if payload.Close == nil || payload.Close.GetReason() == "" {
			return nil, errors.New("terminal close frame is invalid")
		}
		return encodeStatus(statusPayload{Type: "close", Reason: payload.Close.GetReason()})
	default:
		return nil, errors.New("terminal server frame type is unknown")
	}
}

func IsCloseFrame(frame *cubebox.TerminalServerFrame) bool {
	return frame != nil && frame.GetClose() != nil
}

func encodeStatus(status statusPayload) ([]byte, error) {
	payload, err := json.Marshal(status)
	if err != nil {
		return nil, fmt.Errorf("encode terminal status: %w", err)
	}
	if len(payload) > maxStatusBytes {
		return nil, errors.New("terminal status frame exceeds limit")
	}
	message := make([]byte, 1+len(payload))
	message[0] = ChannelStatus
	copy(message[1:], payload)
	return message, nil
}

func decodeStrictJSON(payload []byte, destination interface{}) error {
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
