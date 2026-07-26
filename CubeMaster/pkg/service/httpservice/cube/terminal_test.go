// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTerminalWebSocketRequiresGatewayToken(t *testing.T) {
	t.Setenv(terminalGatewayTokenEnv, "shared-terminal-secret")
	request := httptest.NewRequest(http.MethodGet, "http://master/cube/sandbox/terminal", nil)
	if terminalUpgrader.CheckOrigin(request) {
		t.Fatal("connection without gateway token should be rejected")
	}

	request.Header.Set(terminalGatewayTokenHeader, "wrong-secret")
	if terminalUpgrader.CheckOrigin(request) {
		t.Fatal("connection with wrong gateway token should be rejected")
	}

	request.Header.Set(terminalGatewayTokenHeader, "shared-terminal-secret")
	if !terminalUpgrader.CheckOrigin(request) {
		t.Fatal("internal CubeOps gateway should be accepted")
	}

	request.Header.Set("Origin", "https://dashboard.example.com")
	if terminalUpgrader.CheckOrigin(request) {
		t.Fatal("browser-originated connection should be rejected even with gateway token")
	}
}

func TestTerminalHandlerRejectsInvalidGatewayTokenBeforeUpgrade(t *testing.T) {
	t.Setenv(terminalGatewayTokenEnv, "shared-terminal-secret")
	request := httptest.NewRequest(http.MethodGet, "http://master/cube/sandbox/terminal", nil)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set(terminalGatewayTokenHeader, "wrong-secret")
	response := httptest.NewRecorder()

	handleTerminalAction(response, request, nil)

	if response.Code != http.StatusForbidden {
		t.Fatalf("unexpected status %d, want %d", response.Code, http.StatusForbidden)
	}
}

func TestRunTerminalRelayRecoversPanic(t *testing.T) {
	reason := runTerminalRelay(context.Background(), "test", func() string {
		panic("relay failed")
	})
	if reason != "closed: terminal relay failure" {
		t.Fatalf("unexpected panic close reason %q", reason)
	}
}

func TestNormalizeAndValidateTerminalOpenControl(t *testing.T) {
	valid := &terminalOpenControl{
		Type:        "open",
		SandboxID:   " sandbox ",
		ContainerID: " container ",
		Cols:        80,
		Rows:        24,
	}
	if err := normalizeAndValidateTerminalOpenControl(valid); err != nil {
		t.Fatalf("valid terminal open rejected: %v", err)
	}
	if valid.SandboxID != "sandbox" || valid.ContainerID != "container" {
		t.Fatalf("identifiers were not normalized: %+v", valid)
	}

	for name, open := range map[string]*terminalOpenControl{
		"missing":           nil,
		"wrong type":        {Type: "resize", SandboxID: "sandbox", ContainerID: "container", Cols: 80, Rows: 24},
		"missing sandbox":   {Type: "open", ContainerID: "container", Cols: 80, Rows: 24},
		"missing container": {Type: "open", SandboxID: "sandbox", Cols: 80, Rows: 24},
		"zero dimension":    {Type: "open", SandboxID: "sandbox", ContainerID: "container", Rows: 24},
		"large dimension":   {Type: "open", SandboxID: "sandbox", ContainerID: "container", Cols: 1001, Rows: 24},
		"relative cwd":      {Type: "open", SandboxID: "sandbox", ContainerID: "container", Cols: 80, Rows: 24, Cwd: "tmp"},
		"invalid env":       {Type: "open", SandboxID: "sandbox", ContainerID: "container", Cols: 80, Rows: 24, Env: []string{"INVALID"}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := normalizeAndValidateTerminalOpenControl(open); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}
