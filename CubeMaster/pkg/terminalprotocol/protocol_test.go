// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package terminalprotocol

import (
	"encoding/json"
	"testing"
)

func TestDecodeOpenBindsTargetAndVersion(t *testing.T) {
	raw := []byte(`{"v":1,"type":"open","requestId":"request-1","sessionId":"session-1","sandboxId":"sandbox-1","containerId":"container-1","cols":120,"rows":30}`)
	control, err := DecodeClientControl(raw, StateAwaitingOpen)
	if err != nil {
		t.Fatalf("DecodeClientControl() error = %v", err)
	}
	if control.SandboxID != "sandbox-1" || control.ContainerID != "container-1" {
		t.Fatalf("target = %q/%q", control.SandboxID, control.ContainerID)
	}
}

func TestDecodeClientControlRejectsInvalidStateVersionAndSize(t *testing.T) {
	for _, tc := range []struct {
		name  string
		raw   string
		state State
	}{
		{name: "unsupported version", raw: `{"v":2,"type":"open","sandboxId":"sandbox","containerId":"container","cols":80,"rows":24}`, state: StateAwaitingOpen},
		{name: "resize before open", raw: `{"v":1,"type":"resize","cols":80,"rows":24}`, state: StateAwaitingOpen},
		{name: "open after ready", raw: `{"v":1,"type":"open","sandboxId":"sandbox","containerId":"container","cols":80,"rows":24}`, state: StateReady},
		{name: "oversized terminal", raw: `{"v":1,"type":"open","sandboxId":"sandbox","containerId":"container","cols":501,"rows":24}`, state: StateAwaitingOpen},
		{name: "unknown control", raw: `{"v":1,"type":"retarget","cols":80,"rows":24}`, state: StateReady},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeClientControl([]byte(tc.raw), tc.state); err == nil {
				t.Fatal("DecodeClientControl() accepted invalid control")
			}
		})
	}
}

func TestEncodeServerControlUsesStablePublicShape(t *testing.T) {
	raw, err := EncodeServerControl(ServerControl{
		Version:   Version,
		Type:      TypeError,
		Code:      CodeTargetNotRunning,
		Message:   "target is not running",
		Retryable: false,
	})
	if err != nil {
		t.Fatalf("EncodeServerControl() error = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if got["code"] != string(CodeTargetNotRunning) || got["type"] != string(TypeError) {
		t.Fatalf("encoded control = %#v", got)
	}
}

func TestGatewayTokenMatchesOnlyExactNonEmptyTokens(t *testing.T) {
	if !GatewayTokenMatches("shared-secret", "shared-secret") {
		t.Fatal("matching gateway token rejected")
	}
	for _, pair := range [][2]string{{"", ""}, {"shared-secret", "shared-secreu"}, {"shared-secret", ""}} {
		if GatewayTokenMatches(pair[0], pair[1]) {
			t.Fatalf("GatewayTokenMatches(%q, %q) = true", pair[0], pair[1])
		}
	}
}
