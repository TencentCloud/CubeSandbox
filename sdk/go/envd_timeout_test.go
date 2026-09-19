// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"strings"
	"testing"
)

// A server-side timeout kill reports exitCode:null plus an error. The command
// must not be reported as a success.
func TestParseProcessStartStreamTimeoutIsError(t *testing.T) {
	payload := `{"event":{"end":{"exited":false,"status":"signal: killed",` +
		`"error":"process timed out","termination":{"reason":"timeout"}}}}`
	_, err := parseProcessStartStream(encodeConnectEnvelope([]byte(payload)))
	if err == nil {
		t.Fatal("timed-out command returned nil error")
	}
	if !strings.Contains(err.Error(), "process timed out") {
		t.Fatalf("error %q does not mention the timeout", err)
	}
}

// A missing exit code without an error text is still an error, not success.
func TestParseProcessStartStreamMissingExitCodeIsError(t *testing.T) {
	payload := `{"event":{"end":{"exited":false,"status":"unknown"}}}`
	if _, err := parseProcessStartStream(encodeConnectEnvelope([]byte(payload))); err == nil {
		t.Fatal("EndEvent without exit code returned nil error")
	}
}

func TestParseProcessStartStreamNormalExit(t *testing.T) {
	payload := `{"event":{"end":{"exitCode":0,"exited":true,"status":"exit status 0"}}}`
	result, err := parseProcessStartStream(encodeConnectEnvelope([]byte(payload)))
	if err != nil {
		t.Fatalf("normal exit returned error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", result.ExitCode)
	}
}
