// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubemasterclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestKill_Success(t *testing.T) {
	var capturedMethod, capturedPath string
	var capturedBody killRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &capturedBody)
		_, _ = w.Write([]byte(`{"ret":{"ret_code":200,"ret_msg":"ok"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, time.Second)
	if err := c.Kill(context.Background(), "sbx-1", "cubebox", KillReasonTimeout); err != nil {
		t.Fatalf("Kill returned err: %v", err)
	}
	if capturedMethod != http.MethodDelete {
		t.Fatalf("expected DELETE, got %s", capturedMethod)
	}
	if capturedPath != "/cube/sandbox" {
		t.Fatalf("expected path /cube/sandbox, got %s", capturedPath)
	}
	if capturedBody.SandboxID != "sbx-1" || capturedBody.InstanceType != "cubebox" {
		t.Fatalf("unexpected body: %+v", capturedBody)
	}
	if !capturedBody.Sync {
		t.Fatalf("Kill must request sync=true so the sweeper can evict on success: %+v", capturedBody)
	}
	if capturedBody.KillReason != KillReasonTimeout {
		t.Fatalf("kill_reason should be propagated, got %q", capturedBody.KillReason)
	}
}

func TestPauseSupersededWireContract(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"superseded", `{"ret":{"ret_code":130409,"ret_msg":"auto-pause superseded by lifecycle state change"}}`, true},
		{"lock contention", `{"ret":{"ret_code":130409,"ret_msg":"sandbox lifecycle operation in progress; retry later"}}`, false},
		{"wrong code", `{"ret":{"ret_code":130500,"ret_msg":"auto-pause superseded by lifecycle state change"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			err := New(server.URL, time.Second).Pause(context.Background(), "sbx", "cubebox")
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.IsPauseSuperseded() != tc.want {
				t.Fatalf("unexpected classification: err=%v wantSuperseded=%v", err, tc.want)
			}
		})
	}
}

func TestOnlyAutoPauseCarriesLifecyclePrecondition(t *testing.T) {
	for _, action := range []string{"pause", "resume"} {
		t.Run(action, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				expected, exists := body["expected_lifecycle_state"]
				if action == "pause" && expected != "pausing" {
					t.Error("auto-pause lost precondition")
				}
				if action == "resume" && exists {
					t.Error("resume must not carry pause precondition")
				}
				_, _ = w.Write([]byte(`{"ret":{"ret_code":200}}`))
			}))
			defer server.Close()
			if err := New(server.URL, time.Second).update(context.Background(), "sbx", "cubebox", action); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestResumeCompletedSurvivesErrorDecoding(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ret":{"ret_code":130500,"ret_msg":"state synchronization failed"},"resume_completed":true}`))
	}))
	defer server.Close()
	client := New(server.URL, time.Second)
	var apiErr *APIError
	if err := client.Resume(context.Background(), "sbx", "cubebox"); !errors.As(err, &apiErr) || !apiErr.ResumeCompleted {
		t.Fatalf("partial result lost: %v", err)
	}
	if err := client.Pause(context.Background(), "sbx", "cubebox"); !errors.As(err, &apiErr) || apiErr.ResumeCompleted {
		t.Fatalf("pause misclassified as completed resume: %v", err)
	}
}

func TestSandboxStateRequiresConfirmedTerminalState(t *testing.T) {
	for _, status := range []int{1, 5, 4} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("sandbox_id") != "sbx" || r.URL.Query().Get("instance_type") != "cubebox" {
				t.Error("missing sandbox identity")
			}
			_, _ = fmt.Fprintf(w, `{"ret":{"ret_code":200},"data":[{"sandbox_id":"sbx","status":%d}]}`, status)
		}))
		state, err := New(server.URL, time.Second).SandboxState(context.Background(), "sbx", "cubebox")
		server.Close()
		if status == 4 {
			if err == nil {
				t.Fatal("unconfirmed state accepted")
			}
			continue
		}
		if err != nil || (status == 1 && state != "running") || (status == 5 && state != "paused") {
			t.Fatalf("state=%s err=%v", state, err)
		}
	}
}

func TestKill_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ret":{"ret_code":130483,"ret_msg":"key not found"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, time.Second)
	err := c.Kill(context.Background(), "sbx-1", "cubebox", KillReasonRequest)
	if err == nil {
		t.Fatal("expected error for non-success ret_code")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if !apiErr.IsNotFound() {
		t.Fatalf("expected IsNotFound()=true, got false (%+v)", apiErr)
	}
}

func TestKill_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"ret":{"ret_code":500,"ret_msg":"boom"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, time.Second)
	err := c.Kill(context.Background(), "sbx-1", "cubebox", "")
	if err == nil {
		t.Fatal("expected error on http 500")
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Fatalf("HTTP-level error should NOT be an *APIError, got %v", err)
	}
}

func TestAPIError_IsAlreadyInState(t *testing.T) {
	cases := []struct {
		name string
		err  *APIError
		want bool
	}{
		{"nil", nil, false},
		{"task state invalid", &APIError{RetCode: RetCodeTaskStateInvalid, RetMsg: "sandbox is already paused"}, true},
		{"already has pause snapshot", &APIError{
			RetCode: RetCodeMasterParamsError,
			RetMsg:  "begin pause snapshot: sandbox sbx already has pause snapshot snap-1",
		}, true},
		{"other master params error", &APIError{
			RetCode: RetCodeMasterParamsError,
			RetMsg:  "begin pause snapshot: sandboxID is required",
		}, false},
		{"not found", &APIError{RetCode: RetCodeInvalidParamFormat, RetMsg: "key not found"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.IsAlreadyInState(); got != tc.want {
				t.Fatalf("IsAlreadyInState() = %v, want %v (%+v)", got, tc.want, tc.err)
			}
		})
	}
}

func TestKill_RequiresArgs(t *testing.T) {
	c := New("http://unused", time.Second)
	if err := c.Kill(context.Background(), "", "cubebox", ""); err == nil {
		t.Fatal("Kill must error on empty sandbox_id")
	}
	if err := c.Kill(context.Background(), "sbx", "", ""); err == nil {
		t.Fatal("Kill must error on empty instance_type")
	}
}
