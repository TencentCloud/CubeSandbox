// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package terminalprotocol

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	cubebox "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
)

func TestDecodeClientFrame(t *testing.T) {
	stdin, err := DecodeClientFrame(append([]byte{ChannelStdin}, []byte("echo ready\n")...), 64<<10)
	require.NoError(t, err)
	require.Equal(t, []byte("echo ready\n"), stdin.GetStdin())

	resize, err := DecodeClientFrame(append([]byte{ChannelResize}, []byte(`{"cols":100,"rows":40}`)...), 64<<10)
	require.NoError(t, err)
	require.Equal(t, uint32(100), resize.GetResize().GetCols())
	require.Equal(t, uint32(40), resize.GetResize().GetRows())
}

func TestDecodeClientFrameRejectsUnboundedOrAmbiguousPayloads(t *testing.T) {
	tests := []struct {
		message       []byte
		maxFrameBytes int
	}{
		{message: nil, maxFrameBytes: 5},
		{message: []byte{ChannelStatus}, maxFrameBytes: 5},
		{message: append([]byte{ChannelResize}, []byte(`{"cols":0,"rows":40}`)...), maxFrameBytes: 64 << 10},
		{message: append([]byte{ChannelResize}, []byte(`{"cols":80,"rows":1001}`)...), maxFrameBytes: 64 << 10},
		{message: append([]byte{ChannelResize}, []byte(`{"cols":80,"rows":24,"extra":true}`)...), maxFrameBytes: 64 << 10},
		{message: append([]byte{ChannelResize}, []byte(`{"cols":80,"rows":24}{}`)...), maxFrameBytes: 64 << 10},
		{message: append([]byte{ChannelStdin}, make([]byte, 6)...), maxFrameBytes: 5},
		{message: []byte{0xff}, maxFrameBytes: 64 << 10},
	}
	for _, tt := range tests {
		_, err := DecodeClientFrame(tt.message, tt.maxFrameBytes)
		require.Error(t, err, "message=%q", tt.message)
	}
}

func TestEncodeServerFrame(t *testing.T) {
	opened, err := EncodeServerFrame(&cubebox.TerminalServerFrame{
		Frame: &cubebox.TerminalServerFrame_Opened{Opened: &cubebox.TerminalOpened{
			SessionId:       "session-a",
			ReplayFrom:      12,
			ReplayTruncated: true,
		}},
	}, 64<<10)
	require.NoError(t, err)
	require.Equal(t, ChannelStatus, opened[0])
	var openedStatus map[string]interface{}
	require.NoError(t, json.Unmarshal(opened[1:], &openedStatus))
	require.Equal(t, "opened", openedStatus["type"])
	require.Equal(t, "session-a", openedStatus["sessionId"])
	require.Equal(t, float64(12), openedStatus["replay"].(map[string]interface{})["from"])
	require.Equal(t, true, openedStatus["replay"].(map[string]interface{})["truncated"])

	stdout, err := EncodeServerFrame(&cubebox.TerminalServerFrame{
		Frame: &cubebox.TerminalServerFrame_Stdout{Stdout: &cubebox.TerminalStdout{Data: []byte("hello")}},
	}, 64<<10)
	require.NoError(t, err)
	require.Equal(t, append([]byte{ChannelStdout}, []byte("hello")...), stdout)

	exit, err := EncodeServerFrame(&cubebox.TerminalServerFrame{
		Frame: &cubebox.TerminalServerFrame_Exit{Exit: &cubebox.TerminalExit{ExitCode: 0}},
	}, 64<<10)
	require.NoError(t, err)
	require.JSONEq(t, `{"type":"exit","exitCode":0}`, string(exit[1:]))

	terminalError, err := EncodeServerFrame(&cubebox.TerminalServerFrame{
		Frame: &cubebox.TerminalServerFrame_Error{Error: &cubebox.TerminalError{Code: "LIMIT_EXCEEDED"}},
	}, 64<<10)
	require.NoError(t, err)
	require.JSONEq(t, `{"type":"error","code":"LIMIT_EXCEEDED"}`, string(terminalError[1:]))

	closed, err := EncodeServerFrame(&cubebox.TerminalServerFrame{
		Frame: &cubebox.TerminalServerFrame_Close{Close: &cubebox.TerminalClose{Reason: "USER_CLOSED"}},
	}, 64<<10)
	require.NoError(t, err)
	require.True(t, IsCloseFrame(&cubebox.TerminalServerFrame{
		Frame: &cubebox.TerminalServerFrame_Close{Close: &cubebox.TerminalClose{Reason: "USER_CLOSED"}},
	}))
	require.JSONEq(t, `{"type":"close","reason":"USER_CLOSED"}`, string(closed[1:]))
}

func TestEncodeServerFrameRejectsOversizedStdout(t *testing.T) {
	_, err := EncodeServerFrame(&cubebox.TerminalServerFrame{
		Frame: &cubebox.TerminalServerFrame_Stdout{Stdout: &cubebox.TerminalStdout{Data: []byte("too large")}},
	}, 4)
	require.Error(t, err)
}

func TestDecodeClientFrameLimitIsPayloadBytes(t *testing.T) {
	message := append([]byte{ChannelStdin}, []byte("12345")...)
	frame, err := DecodeClientFrame(message, 5)
	require.NoError(t, err)
	require.Equal(t, []byte("12345"), frame.GetStdin())

	_, err = DecodeClientFrame(append(message, '6'), 5)
	require.Error(t, err)
}

func TestProtocolRejectsInvalidFrameLimit(t *testing.T) {
	_, err := DecodeClientFrame([]byte{ChannelStdin}, 0)
	require.Error(t, err)

	_, err = EncodeServerFrame(&cubebox.TerminalServerFrame{
		Frame: &cubebox.TerminalServerFrame_Exit{Exit: &cubebox.TerminalExit{}},
	}, 0)
	require.Error(t, err)
}

func TestDecodeClientFrameRejectsServerOwnedAndReservedChannels(t *testing.T) {
	for _, channel := range []byte{ChannelStdout, ChannelStderr, ChannelStatus} {
		_, err := DecodeClientFrame([]byte{channel}, 64<<10)
		require.Error(t, err, "channel=0x%02x", channel)
	}
}

func TestEncodeServerFrameRejectsInvalidFrames(t *testing.T) {
	tests := []*cubebox.TerminalServerFrame{
		nil,
		{},
		{Frame: &cubebox.TerminalServerFrame_Opened{}},
		{Frame: &cubebox.TerminalServerFrame_Stdout{}},
		{Frame: &cubebox.TerminalServerFrame_Error{Error: &cubebox.TerminalError{}}},
		{Frame: &cubebox.TerminalServerFrame_Close{Close: &cubebox.TerminalClose{}}},
	}
	for _, frame := range tests {
		_, err := EncodeServerFrame(frame, 64<<10)
		require.Error(t, err)
	}
}

func TestEncodeServerFrameRejectsOversizedStatus(t *testing.T) {
	_, err := EncodeServerFrame(&cubebox.TerminalServerFrame{
		Frame: &cubebox.TerminalServerFrame_Opened{Opened: &cubebox.TerminalOpened{
			SessionId: string(make([]byte, maxStatusBytes)),
		}},
	}, 64<<10)
	require.Error(t, err)
}
