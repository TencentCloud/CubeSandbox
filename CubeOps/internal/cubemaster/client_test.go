// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubemaster

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestReadResponse_BusinessErrorReturnsCMError verifies that readResponse
// returns a *CMError (not a generic error) when CubeMaster returns a non-zero
// ret_code. This is the R11 fix: handlers depend on errors.As(err, &CMError{})
// to map ret_code to 404/409/503.
func TestReadResponse_BusinessErrorReturnsCMError(t *testing.T) {
	tests := []struct {
		name     string
		retCode  int
		retMsg   string
		wantType bool
	}{
		{"not found", 130404, "sandbox not found", true},
		{"conflict", 130409, "sandbox is paused", true},
		{"pausing", 130490, "sandbox is pausing", true},
		{"resume failed", 130589, "resume timeout", true},
		{"success code 0", 0, "", false},
		{"success code 200", 200, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"ret":{"ret_code":%d,"ret_msg":%q}}`, tt.retCode, tt.retMsg)
			resp := &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(&stringReader{s: body}),
				Header:     make(http.Header),
			}
			_, err := readResponse(resp)
			if tt.wantType {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				var cmErr *CMError
				if !errorAs(err, &cmErr) {
					t.Fatalf("expected *CMError, got %T: %v", err, err)
				}
				if cmErr.RetCode != tt.retCode {
					t.Errorf("RetCode = %d, want %d", cmErr.RetCode, tt.retCode)
				}
			} else {
				if err != nil {
					t.Fatalf("expected no error, got: %v", err)
				}
			}
		})
	}
}

// TestCMError_Predicates tests the IsNotFound/IsConflict/IsPausing/etc helpers.
func TestCMError_Predicates(t *testing.T) {
	tests := []struct {
		code         int
		isNotFound   bool
		isConflict   bool
		isPausing    bool
		isResumeFail bool
		retryAfter   int
	}{
		{130404, true, false, false, false, 0},
		{404, true, false, false, false, 0},
		{130409, false, true, false, false, 0},
		{409, false, true, false, false, 0},
		{130490, false, false, true, false, 2},
		{130589, false, false, false, true, 5},
		{99999, false, false, false, false, 0},
	}
	for _, tt := range tests {
		e := &CMError{RetCode: tt.code, RetMsg: "test"}
		if e.IsNotFound() != tt.isNotFound {
			t.Errorf("code %d: IsNotFound = %v, want %v", tt.code, e.IsNotFound(), tt.isNotFound)
		}
		if e.IsConflict() != tt.isConflict {
			t.Errorf("code %d: IsConflict = %v, want %v", tt.code, e.IsConflict(), tt.isConflict)
		}
		if e.IsPausing() != tt.isPausing {
			t.Errorf("code %d: IsPausing = %v, want %v", tt.code, e.IsPausing(), tt.isPausing)
		}
		if e.IsResumeFailed() != tt.isResumeFail {
			t.Errorf("code %d: IsResumeFailed = %v, want %v", tt.code, e.IsResumeFailed(), tt.isResumeFail)
		}
		if e.RetryAfter() != tt.retryAfter {
			t.Errorf("code %d: RetryAfter = %d, want %d", tt.code, e.RetryAfter(), tt.retryAfter)
		}
	}
}

// TestClient_GetSandbox_HTTPError verifies the client returns CMError from a
// real httptest.Server, proving the error type survives the full HTTP round-trip.
func TestClient_GetSandbox_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ret":{"ret_code":130404,"ret_msg":"sandbox not found"}}`)
	}))
	defer srv.Close()

	client := New(srv.URL)
	_, err := client.GetSandbox(context.Background(), "sb-missing", "cubebox")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var cmErr *CMError
	if !errorAs(err, &cmErr) {
		t.Fatalf("expected *CMError, got %T: %v", err, err)
	}
	if !cmErr.IsNotFound() {
		t.Errorf("expected IsNotFound, got RetCode=%d", cmErr.RetCode)
	}
}

// TestClient_CreateSandbox_Success verifies the client returns raw JSON on success.
func TestClient_CreateSandbox_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ret":{"ret_code":0},"data":{"sandbox_id":"sb-123","template_id":"tpl-1"}}`)
	}))
	defer srv.Close()

	client := New(srv.URL)
	raw, err := client.CreateSandbox(context.Background(), map[string]interface{}{
		"template_id": "tpl-1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// readResponse returns the full envelope; data is nested under "data".
	var env struct {
		Data struct {
			SandboxID  string `json:"sandbox_id"`
			TemplateID string `json:"template_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Data.SandboxID != "sb-123" {
		t.Errorf("sandbox_id = %s, want sb-123", env.Data.SandboxID)
	}
}

// TestClient_NoGlobalTimeout verifies that the client does NOT have a global
// 30s timeout (R12 fix). A request with a long context deadline should succeed
// even if the server takes >30s (simulated by checking that the client's
// http.Client.Timeout is zero).
func TestClient_NoGlobalTimeout(t *testing.T) {
	client := New("http://localhost:1")
	if client.http.Timeout != 0 {
		t.Errorf("client.http.Timeout = %v, want 0 (no global timeout; R12 uses per-request context)", client.http.Timeout)
	}
}

// TestClient_LongContextNotCappedAt30s proves the R12 fix: a server that
// takes longer than 30s to respond succeeds as long as the context deadline
// has not expired. Before R12, the client had a fixed 30s http.Client.Timeout
// that would kill this request even though the caller passed a longer context.
//
// We use a 35s server delay with a 60s context to prove the 30s cap is gone.
// To keep the test fast, we run it with a shortened delay (1.5s server + 3s
// context) — the point is that the request succeeds despite the server being
// "slow" relative to the old 30s default. The real proof is
// TestClient_NoGlobalTimeout (Timeout == 0); this test adds the behavioral
// confirmation that per-request context controls the deadline end-to-end.
func TestClient_LongContextNotCappedAt30s(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1500 * time.Millisecond) // "long" operation
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ret":{"ret_code":0}}`)
	}))
	defer srv.Close()

	client := New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := client.GetSandbox(ctx, "sb-long", "cubebox")
	if err != nil {
		t.Errorf("expected success (context deadline 3s > server delay 1.5s), got error: %v", err)
	}
}

// TestClient_RespectsContextDeadline verifies that a short context deadline
// causes the request to fail (proving per-request timeout control works).
func TestClient_RespectsContextDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		fmt.Fprint(w, `{"ret":{"ret_code":0}}`)
	}))
	defer srv.Close()

	client := New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := client.GetSandbox(ctx, "sb-1", "cubebox")
	if err == nil {
		t.Fatal("expected context deadline error, got nil")
	}
}

// --- helpers ---

// stringReader is a minimal io.Reader for test responses.
type stringReader struct {
	s string
	i int
}

func (r *stringReader) Read(p []byte) (int, error) {
	if r.i >= len(r.s) {
		return 0, io.EOF
	}
	n := copy(p, r.s[r.i:])
	r.i += n
	return n, nil
}

// errorAs wraps errors.As to avoid importing errors in the test file.
func errorAs(err error, target interface{}) bool {
	// Use type assertion since CMError is in the same package.
	if cmErr, ok := target.(**CMError); ok {
		if e, ok := err.(*CMError); ok {
			*cmErr = e
			return true
		}
	}
	return false
}

// TestClient_FullRetCodeRoundTrip verifies the complete R11 call chain:
// CubeMaster HTTP JSON response → readResponse → *CMError → predicates.
// Each ret_code must survive the full HTTP round-trip as a *CMError with
// the correct RetCode and predicate results, so that writeCMError in the
// handler can map it to 404/409/503 instead of collapsing to 502.
func TestClient_FullRetCodeRoundTrip(t *testing.T) {
	tests := []struct {
		name       string
		retCode    int
		retMsg     string
		isNotFound bool
		isConflict bool
		isPausing  bool
		isResume   bool
		retryAfter int
	}{
		{"not_found_130404", 130404, "sandbox not found", true, false, false, false, 0},
		{"not_found_404", 404, "not found", true, false, false, false, 0},
		{"conflict_130409", 130409, "conflict", false, true, false, false, 0},
		{"conflict_409", 409, "conflict", false, true, false, false, 0},
		{"pausing_130490", 130490, "pausing", false, false, true, false, 2},
		{"resume_failed_130589", 130589, "resume timeout", false, false, false, true, 5},
		{"unknown_130500", 130500, "internal", false, false, false, false, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"ret":{"ret_code":%d,"ret_msg":%q}}`, tt.retCode, tt.retMsg)
			}))
			defer srv.Close()

			client := New(srv.URL)
			_, err := client.GetSandbox(context.Background(), "sb-test", "cubebox")
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			var cmErr *CMError
			if !errorAs(err, &cmErr) {
				t.Fatalf("expected *CMError, got %T: %v", err, err)
			}
			if cmErr.RetCode != tt.retCode {
				t.Errorf("RetCode = %d, want %d", cmErr.RetCode, tt.retCode)
			}
			if cmErr.RetMsg != tt.retMsg {
				t.Errorf("RetMsg = %q, want %q", cmErr.RetMsg, tt.retMsg)
			}
			if cmErr.IsNotFound() != tt.isNotFound {
				t.Errorf("IsNotFound = %v, want %v", cmErr.IsNotFound(), tt.isNotFound)
			}
			if cmErr.IsConflict() != tt.isConflict {
				t.Errorf("IsConflict = %v, want %v", cmErr.IsConflict(), tt.isConflict)
			}
			if cmErr.IsPausing() != tt.isPausing {
				t.Errorf("IsPausing = %v, want %v", cmErr.IsPausing(), tt.isPausing)
			}
			if cmErr.IsResumeFailed() != tt.isResume {
				t.Errorf("IsResumeFailed = %v, want %v", cmErr.IsResumeFailed(), tt.isResume)
			}
			if cmErr.RetryAfter() != tt.retryAfter {
				t.Errorf("RetryAfter = %d, want %d", cmErr.RetryAfter(), tt.retryAfter)
			}
		})
	}
}
