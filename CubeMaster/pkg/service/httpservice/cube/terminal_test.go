// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cube

import (
	"net/http/httptest"
	"testing"
)

func TestTerminalGatewayAuthorization(t *testing.T) {
	request := httptest.NewRequest("GET", "/cube/sandbox/terminal/ws", nil)
	if terminalGatewayAuthorizedWithToken(request, "") {
		t.Fatal("empty configured token must disable the terminal endpoint")
	}

	request.Header.Set("X-Cube-Terminal-Gateway", "wrong")
	if terminalGatewayAuthorizedWithToken(request, "expected") {
		t.Fatal("mismatched gateway token must be rejected")
	}

	request.Header.Set("X-Cube-Terminal-Gateway", "expected")
	if !terminalGatewayAuthorizedWithToken(request, "expected") {
		t.Fatal("matching gateway token must be accepted")
	}
}
