// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import "testing"

func TestTerminalProtocolUsesDirectionSpecificFrames(t *testing.T) {
	client := &TerminalClientMessage{
		Payload: &TerminalClientMessage_Open{Open: &TerminalOpenRequest{
			RequestId:   "request-1",
			SessionId:   "session-1",
			SandboxId:   "sandbox-1",
			ContainerId: "container-1",
			Cols:        120,
			Rows:        30,
		}},
	}
	if got := client.GetOpen().GetSessionId(); got != "session-1" {
		t.Fatalf("client open session_id = %q, want session-1", got)
	}

	server := &TerminalServerMessage{
		Payload: &TerminalServerMessage_Ready{Ready: &TerminalReady{ExecId: "exec-1"}},
	}
	if got := server.GetReady().GetExecId(); got != "exec-1" {
		t.Fatalf("server ready exec_id = %q, want exec-1", got)
	}
}

func TestTerminalProtocolExposesStableErrorCode(t *testing.T) {
	message := &TerminalServerMessage{
		Payload: &TerminalServerMessage_Error{Error: &TerminalError{
			Code:      TerminalErrorCode_TERMINAL_ERROR_TARGET_NOT_RUNNING,
			Message:   "target is not running",
			Retryable: false,
		}},
	}

	if got := message.GetError().GetCode(); got != TerminalErrorCode_TERMINAL_ERROR_TARGET_NOT_RUNNING {
		t.Fatalf("terminal error code = %v", got)
	}
}

func TestTerminalRPCMethodNameIsStable(t *testing.T) {
	const want = "/cubelet.services.cubebox.v1.CubeboxMgr/AttachTerminal"
	if CubeboxMgr_AttachTerminal_FullMethodName != want {
		t.Fatalf("AttachTerminal method = %q, want %q", CubeboxMgr_AttachTerminal_FullMethodName, want)
	}
}
