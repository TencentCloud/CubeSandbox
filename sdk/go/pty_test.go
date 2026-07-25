// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newPtyTestSandbox(t *testing.T, server *httptest.Server) *Sandbox {
	t.Helper()
	host, port := serverHostPort(t, server.URL)
	client := NewClient(Config{
		ProxyNodeIP:    host,
		ProxyPortHTTP:  port,
		SandboxDomain:  "cube.test",
		RequestTimeout: 5 * time.Second,
	})
	return &Sandbox{client: client, SandboxID: "sb-pty", EnvdAccessToken: "tok"}
}

// ptyDataFrame builds a Connect data frame carrying base64-encoded PTY output.
func ptyDataFrame(text string) []byte {
	encoded := base64.StdEncoding.EncodeToString([]byte(text))
	return connectEnvelope(0, `{"event":{"data":{"pty":"`+encoded+`"}}}`)
}

// decodeEnvelopeJSON strips the 5-byte Connect envelope header and unmarshals
// the JSON body into v.
func decodeEnvelopeJSON(t *testing.T, body []byte, v any) {
	t.Helper()
	if len(body) < 5 {
		t.Fatalf("body too short for Connect envelope: %d bytes", len(body))
	}
	if err := json.Unmarshal(body[5:], v); err != nil {
		t.Fatalf("decode enveloped body: %v", err)
	}
}

func TestPtyCreateStreamsAndWaits(t *testing.T) {
	var gotPath, gotCT, gotAuth, gotToken string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		gotAuth = r.Header.Get("Authorization")
		gotToken = r.Header.Get("X-Access-Token")
		gotBody, _ = io.ReadAll(r.Body)

		w.Header().Set("Content-Type", connectContentType)
		w.Write(connectEnvelope(0, `{"event":{"start":{"pid":4321}}}`))
		w.Write(ptyDataFrame("hello "))
		w.Write(ptyDataFrame("world"))
		w.Write(connectEnvelope(0, `{"event":{"end":{"exitCode":0,"exited":true}}}`))
		w.Write(connectEnvelope(connectEndStreamFlag, `{}`))
	}))
	defer server.Close()

	sb := newPtyTestSandbox(t, server)
	handle, err := sb.Pty().Create(context.Background(), PtySize{Rows: 24, Cols: 80}, PtyCreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if handle.PID() != 4321 {
		t.Fatalf("PID=%d, want 4321", handle.PID())
	}

	var buf bytes.Buffer
	code, err := handle.Wait(func(chunk []byte) { buf.Write(chunk) })
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit code=%d, want 0", code)
	}
	if buf.String() != "hello world" {
		t.Fatalf("output=%q, want %q", buf.String(), "hello world")
	}
	if got, ok := handle.ExitCode(); !ok || got != 0 {
		t.Fatalf("ExitCode=(%d,%v), want (0,true)", got, ok)
	}

	if gotPath != "/process.Process/Start" {
		t.Fatalf("path=%q", gotPath)
	}
	if gotCT != connectContentType {
		t.Fatalf("content-type=%q, want %q", gotCT, connectContentType)
	}
	if gotAuth != basicAuthUser("root") {
		t.Fatalf("Authorization=%q, want root basic auth", gotAuth)
	}
	if gotToken != "tok" {
		t.Fatalf("X-Access-Token=%q, want tok", gotToken)
	}

	var req ptyStartRequest
	decodeEnvelopeJSON(t, gotBody, &req)
	if req.Process.Cmd != "/bin/bash" {
		t.Fatalf("cmd=%q", req.Process.Cmd)
	}
	if strings.Join(req.Process.Args, " ") != "-i -l" {
		t.Fatalf("args=%v, want [-i -l]", req.Process.Args)
	}
	if req.PTY.Size.Rows != 24 || req.PTY.Size.Cols != 80 {
		t.Fatalf("size=%+v, want {24 80}", req.PTY.Size)
	}
	if req.Process.Envs["TERM"] != "xterm-256color" {
		t.Fatalf("TERM=%q, want xterm-256color", req.Process.Envs["TERM"])
	}
	if req.Process.Envs["LANG"] != "C.UTF-8" || req.Process.Envs["LC_ALL"] != "C.UTF-8" {
		t.Fatalf("locale envs=%v", req.Process.Envs)
	}
}

func TestPtyCreateUserEnvsCwd(t *testing.T) {
	var gotAuth string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", connectContentType)
		w.Write(connectEnvelope(0, `{"event":{"start":{"pid":10}}}`))
		w.Write(connectEnvelope(0, `{"event":{"end":{"exitCode":0,"exited":true}}}`))
		w.Write(connectEnvelope(connectEndStreamFlag, `{}`))
	}))
	defer server.Close()

	sb := newPtyTestSandbox(t, server)
	handle, err := sb.Pty().Create(context.Background(), PtySize{Rows: 10, Cols: 40}, PtyCreateOptions{
		User: "app",
		Cwd:  "/work",
		Envs: map[string]string{"TERM": "vt100", "FOO": "bar"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := handle.Wait(nil); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	if gotAuth != basicAuthUser("app") {
		t.Fatalf("Authorization=%q, want app basic auth", gotAuth)
	}
	var req ptyStartRequest
	decodeEnvelopeJSON(t, gotBody, &req)
	if req.Process.Cwd != "/work" {
		t.Fatalf("cwd=%q", req.Process.Cwd)
	}
	if req.Process.Envs["TERM"] != "vt100" {
		t.Fatalf("TERM override lost: %q", req.Process.Envs["TERM"])
	}
	if req.Process.Envs["FOO"] != "bar" {
		t.Fatalf("FOO=%q", req.Process.Envs["FOO"])
	}
	if req.Process.Envs["LANG"] != "C.UTF-8" {
		t.Fatalf("LANG default lost: %q", req.Process.Envs["LANG"])
	}
}

func TestPtyWaitSurfacesEndError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", connectContentType)
		w.Write(connectEnvelope(0, `{"event":{"start":{"pid":7}}}`))
		w.Write(connectEnvelope(0, `{"event":{"end":{"error":"signal: killed"}}}`))
		w.Write(connectEnvelope(connectEndStreamFlag, `{}`))
	}))
	defer server.Close()

	sb := newPtyTestSandbox(t, server)
	handle, err := sb.Pty().Create(context.Background(), PtySize{Rows: 24, Cols: 80}, PtyCreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err = handle.Wait(nil)
	if err == nil || !strings.Contains(err.Error(), "signal: killed") {
		t.Fatalf("Wait error=%v, want it to mention 'signal: killed'", err)
	}
	if handle.ErrorMessage() != "signal: killed" {
		t.Fatalf("ErrorMessage=%q", handle.ErrorMessage())
	}
}

func TestPtyConnectNoAuthHeader(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", connectContentType)
		w.Write(connectEnvelope(0, `{"event":{"start":{"pid":99}}}`))
		w.Write(connectEnvelope(0, `{"event":{"end":{"exitCode":0,"exited":true}}}`))
		w.Write(connectEnvelope(connectEndStreamFlag, `{}`))
	}))
	defer server.Close()

	sb := newPtyTestSandbox(t, server)
	handle, err := sb.Pty().Connect(context.Background(), 99, PtyConnectOptions{})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if handle.PID() != 99 {
		t.Fatalf("PID=%d, want 99", handle.PID())
	}
	if _, err := handle.Wait(nil); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if gotPath != "/process.Process/Connect" {
		t.Fatalf("path=%q", gotPath)
	}
	if gotAuth != "" {
		t.Fatalf("Connect should not send Authorization, got %q", gotAuth)
	}
	var req ptySelectorRequest
	decodeEnvelopeJSON(t, gotBody, &req)
	if req.Process.PID != 99 {
		t.Fatalf("selector pid=%d, want 99", req.Process.PID)
	}
}

func TestPtyStartHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	}))
	defer server.Close()

	sb := newPtyTestSandbox(t, server)
	_, err := sb.Pty().Create(context.Background(), PtySize{Rows: 1, Cols: 1}, PtyCreateOptions{})
	var apiErr *APIError
	if err == nil {
		t.Fatal("Create returned nil error")
	}
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("Create error=%v, want APIError 500", err)
	}
}

func TestPtyKill(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		var gotPath, gotCT string
		var gotBody []byte
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			gotCT = r.Header.Get("Content-Type")
			gotBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`))
		}))
		defer server.Close()

		sb := newPtyTestSandbox(t, server)
		killed, err := sb.Pty().Kill(context.Background(), 42)
		if err != nil {
			t.Fatalf("Kill: %v", err)
		}
		if !killed {
			t.Fatal("Kill returned false, want true")
		}
		if gotPath != "/process.Process/SendSignal" {
			t.Fatalf("path=%q", gotPath)
		}
		if gotCT != "application/json" {
			t.Fatalf("content-type=%q, want application/json", gotCT)
		}
		var req ptySignalRequest
		if err := json.Unmarshal(gotBody, &req); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if req.Process.PID != 42 || req.Signal != signalSIGKILL {
			t.Fatalf("signal req=%+v", req)
		}
	})

	t.Run("http 404 is not found", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"no such process"}`, http.StatusNotFound)
		}))
		defer server.Close()

		sb := newPtyTestSandbox(t, server)
		killed, err := sb.Pty().Kill(context.Background(), 42)
		if err != nil {
			t.Fatalf("Kill returned error on 404: %v", err)
		}
		if killed {
			t.Fatal("Kill returned true on 404, want false")
		}
	})

	t.Run("connect not_found body is not found", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"code":"not_found","message":"gone"}`))
		}))
		defer server.Close()

		sb := newPtyTestSandbox(t, server)
		killed, err := sb.Pty().Kill(context.Background(), 42)
		if err != nil {
			t.Fatalf("Kill returned error on not_found body: %v", err)
		}
		if killed {
			t.Fatal("Kill returned true on not_found body, want false")
		}
	})

	t.Run("other error propagates", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
		}))
		defer server.Close()

		sb := newPtyTestSandbox(t, server)
		if _, err := sb.Pty().Kill(context.Background(), 42); err == nil {
			t.Fatal("Kill returned nil error on 500")
		}
	})
}

func TestPtySendStdin(t *testing.T) {
	var gotPath, gotCT string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sb := newPtyTestSandbox(t, server)
	if err := sb.Pty().SendStdin(context.Background(), 5, []byte("ls -la\n")); err != nil {
		t.Fatalf("SendStdin: %v", err)
	}
	if gotPath != "/process.Process/SendInput" {
		t.Fatalf("path=%q", gotPath)
	}
	if gotCT != "application/json" {
		t.Fatalf("content-type=%q", gotCT)
	}
	var req ptyInputRequest
	if err := json.Unmarshal(gotBody, &req); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if req.Process.PID != 5 {
		t.Fatalf("pid=%d, want 5", req.Process.PID)
	}
	decoded, err := base64.StdEncoding.DecodeString(req.Input.PTY)
	if err != nil {
		t.Fatalf("input.pty not base64: %v", err)
	}
	if string(decoded) != "ls -la\n" {
		t.Fatalf("input=%q, want %q", decoded, "ls -la\n")
	}
}

func TestPtyResize(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sb := newPtyTestSandbox(t, server)
	if err := sb.Pty().Resize(context.Background(), 8, PtySize{Rows: 30, Cols: 120}); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if gotPath != "/process.Process/Update" {
		t.Fatalf("path=%q", gotPath)
	}
	var req ptyUpdateRequest
	if err := json.Unmarshal(gotBody, &req); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if req.Process.PID != 8 || req.PTY.Size.Rows != 30 || req.PTY.Size.Cols != 120 {
		t.Fatalf("update req=%+v", req)
	}
}

func TestPtyHandleDelegatesToHandlePID(t *testing.T) {
	var stdinPID int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/process.Process/Start":
			w.Header().Set("Content-Type", connectContentType)
			w.Write(connectEnvelope(0, `{"event":{"start":{"pid":314}}}`))
			w.Write(connectEnvelope(0, `{"event":{"end":{"exitCode":0,"exited":true}}}`))
			w.Write(connectEnvelope(connectEndStreamFlag, `{}`))
		case "/process.Process/SendInput":
			body, _ := io.ReadAll(r.Body)
			var req ptyInputRequest
			json.Unmarshal(body, &req)
			stdinPID = req.Process.PID
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	sb := newPtyTestSandbox(t, server)
	handle, err := sb.Pty().Create(context.Background(), PtySize{Rows: 24, Cols: 80}, PtyCreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := handle.SendStdin(context.Background(), []byte("x")); err != nil {
		t.Fatalf("handle.SendStdin: %v", err)
	}
	if stdinPID != handle.PID() {
		t.Fatalf("SendInput pid=%d, want handle pid %d", stdinPID, handle.PID())
	}
	if _, err := handle.Wait(nil); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func TestPtyIdleTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", connectContentType)
		w.Write(connectEnvelope(0, `{"event":{"start":{"pid":5}}}`))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Go quiet: send no further frames so the client-side idle timer fires.
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer server.Close()

	sb := newPtyTestSandbox(t, server)
	handle, err := sb.Pty().Create(context.Background(), PtySize{Rows: 24, Cols: 80}, PtyCreateOptions{
		Timeout: 150 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	done := make(chan struct{})
	var werr error
	go func() {
		_, werr = handle.Wait(nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Wait did not return after idle timeout")
	}
	if werr == nil || !strings.Contains(werr.Error(), "timed out") {
		t.Fatalf("Wait error=%v, want an idle timeout error", werr)
	}
}

func TestPtyDisconnectKeepsStreamOpen(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", connectContentType)
		w.Write(connectEnvelope(0, `{"event":{"start":{"pid":55}}}`))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer server.Close()

	sb := newPtyTestSandbox(t, server)
	handle, err := sb.Pty().Create(context.Background(), PtySize{Rows: 24, Cols: 80}, PtyCreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if handle.PID() != 55 {
		t.Fatalf("PID=%d, want 55", handle.PID())
	}
	if err := handle.Disconnect(); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	// Second Disconnect must be a no-op, not a panic on double-close.
	if err := handle.Disconnect(); err != nil {
		t.Fatalf("second Disconnect: %v", err)
	}

	done := make(chan struct{})
	var werr error
	go func() {
		_, werr = handle.Wait(nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Wait did not return after Disconnect")
	}
	if werr == nil || !strings.Contains(werr.Error(), "without an end event") {
		t.Fatalf("Wait after Disconnect error=%v, want 'without an end event'", werr)
	}
}

func TestExitCodeFromStatus(t *testing.T) {
	// -1 is accepted on purpose: the regex mirrors the Python/Node SDKs
	// (`-?\d+`) so the three SDKs parse envd's free-form status identically.
	cases := []struct {
		name     string
		status   string
		wantCode int
		wantOK   bool
	}{
		{"empty", "", 0, false},
		{"exit status", "exit status 3", 3, true},
		{"exited with code", "exited with code 7", 7, true},
		{"signal", "signal 9", 137, true},
		{"terminated by signal", "terminated by signal 15", 143, true},
		{"plain exited", "exited", 0, true},
		{"negative parity", "exit status -1", -1, true},
		{"unrecognized", "still running", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := exitCodeFromStatus(tc.status)
			if got != tc.wantCode || ok != tc.wantOK {
				t.Fatalf("exitCodeFromStatus(%q)=(%d,%v), want (%d,%v)", tc.status, got, ok, tc.wantCode, tc.wantOK)
			}
		})
	}
}

// TestPtyRecordEndFallbacks exercises recordEnd's non-exitCode branches: exit
// code parsed from the free-form "status" string, and the "exited" flag with no
// code (defaults to 0).
func TestPtyRecordEndFallbacks(t *testing.T) {
	t.Run("status string", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", connectContentType)
			w.Write(connectEnvelope(0, `{"event":{"start":{"pid":7}}}`))
			w.Write(connectEnvelope(0, `{"event":{"end":{"status":"exit status 5","exited":true}}}`))
			w.Write(connectEnvelope(connectEndStreamFlag, `{}`))
		}))
		defer server.Close()

		sb := newPtyTestSandbox(t, server)
		handle, err := sb.Pty().Create(context.Background(), PtySize{Rows: 24, Cols: 80}, PtyCreateOptions{})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		code, err := handle.Wait(nil)
		if err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if code != 5 {
			t.Fatalf("exit code=%d, want 5 (parsed from status)", code)
		}
	})

	t.Run("exited flag only", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", connectContentType)
			w.Write(connectEnvelope(0, `{"event":{"start":{"pid":7}}}`))
			w.Write(connectEnvelope(0, `{"event":{"end":{"exited":true}}}`))
			w.Write(connectEnvelope(connectEndStreamFlag, `{}`))
		}))
		defer server.Close()

		sb := newPtyTestSandbox(t, server)
		handle, err := sb.Pty().Create(context.Background(), PtySize{Rows: 24, Cols: 80}, PtyCreateOptions{})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		code, err := handle.Wait(nil)
		if err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if got, ok := handle.ExitCode(); !ok || got != 0 || code != 0 {
			t.Fatalf("ExitCode=(%d,%v), Wait code=%d, want 0/true/0", got, ok, code)
		}
	})
}

// TestPtyOutputChannelStreaming consumes output by ranging Output() directly,
// the alternative to Wait's callback.
func TestPtyOutputChannelStreaming(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", connectContentType)
		w.Write(connectEnvelope(0, `{"event":{"start":{"pid":8}}}`))
		w.Write(ptyDataFrame("hello "))
		w.Write(ptyDataFrame("world"))
		w.Write(connectEnvelope(0, `{"event":{"end":{"exitCode":0,"exited":true}}}`))
		w.Write(connectEnvelope(connectEndStreamFlag, `{}`))
	}))
	defer server.Close()

	sb := newPtyTestSandbox(t, server)
	handle, err := sb.Pty().Create(context.Background(), PtySize{Rows: 24, Cols: 80}, PtyCreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	var buf bytes.Buffer
	for chunk := range handle.Output() {
		buf.Write(chunk)
	}
	if buf.String() != "hello world" {
		t.Fatalf("output=%q, want %q", buf.String(), "hello world")
	}
}

func TestPtyCompressedFrameError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", connectContentType)
		w.Write(connectEnvelope(0, `{"event":{"start":{"pid":9}}}`))
		w.Write(connectEnvelope(connectCompressedFlag, `{}`))
	}))
	defer server.Close()

	sb := newPtyTestSandbox(t, server)
	handle, err := sb.Pty().Create(context.Background(), PtySize{Rows: 24, Cols: 80}, PtyCreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err = handle.Wait(nil)
	if err == nil || !strings.Contains(err.Error(), "compressed") {
		t.Fatalf("Wait error=%v, want a compressed-frame error", err)
	}
}

func TestPtyEndStreamTrailerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", connectContentType)
		w.Write(connectEnvelope(0, `{"event":{"start":{"pid":10}}}`))
		w.Write(connectEnvelope(connectEndStreamFlag, `{"error":{"code":"internal","message":"boom"}}`))
	}))
	defer server.Close()

	sb := newPtyTestSandbox(t, server)
	handle, err := sb.Pty().Create(context.Background(), PtySize{Rows: 24, Cols: 80}, PtyCreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err = handle.Wait(nil)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Wait error=%v, want the trailer error 'boom'", err)
	}
}

func TestPtyBase64DecodeError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", connectContentType)
		w.Write(connectEnvelope(0, `{"event":{"start":{"pid":11}}}`))
		w.Write(connectEnvelope(0, `{"event":{"data":{"pty":"@@not-base64@@"}}}`))
	}))
	defer server.Close()

	sb := newPtyTestSandbox(t, server)
	handle, err := sb.Pty().Create(context.Background(), PtySize{Rows: 24, Cols: 80}, PtyCreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err = handle.Wait(nil)
	if err == nil || !strings.Contains(err.Error(), "decode pty output") {
		t.Fatalf("Wait error=%v, want a base64 decode error", err)
	}
}

func TestPtyStartStreamClosedBeforeStart(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", connectContentType)
		// End the stream immediately, before any start event.
		w.Write(connectEnvelope(connectEndStreamFlag, `{}`))
	}))
	defer server.Close()

	sb := newPtyTestSandbox(t, server)
	_, err := sb.Pty().Create(context.Background(), PtySize{Rows: 1, Cols: 1}, PtyCreateOptions{})
	if err == nil || !strings.Contains(err.Error(), "stream closed before start event") {
		t.Fatalf("Create error=%v, want 'stream closed before start event'", err)
	}
}

func TestPtyConnectHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"nope"}`, http.StatusInternalServerError)
	}))
	defer server.Close()

	sb := newPtyTestSandbox(t, server)
	_, err := sb.Pty().Connect(context.Background(), 1, PtyConnectOptions{})
	var apiErr *APIError
	if err == nil || !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("Connect error=%v, want APIError 500", err)
	}
}

func TestPtySendStdinAndResizeErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	}))
	defer server.Close()

	sb := newPtyTestSandbox(t, server)
	if err := sb.Pty().SendStdin(context.Background(), 5, []byte("x")); err == nil {
		t.Fatal("SendStdin returned nil error on 500")
	}
	if err := sb.Pty().Resize(context.Background(), 5, PtySize{Rows: 1, Cols: 1}); err == nil {
		t.Fatal("Resize returned nil error on 500")
	}
}

// TestPtyHandleKillResizeDelegation verifies the per-handle Kill and Resize
// shortcuts target the handle's own PID (SendStdin is covered separately).
func TestPtyHandleKillResizeDelegation(t *testing.T) {
	var killPID, resizePID int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/process.Process/Start":
			w.Header().Set("Content-Type", connectContentType)
			w.Write(connectEnvelope(0, `{"event":{"start":{"pid":271}}}`))
			w.Write(connectEnvelope(0, `{"event":{"end":{"exitCode":0,"exited":true}}}`))
			w.Write(connectEnvelope(connectEndStreamFlag, `{}`))
		case "/process.Process/SendSignal":
			var req ptySignalRequest
			json.Unmarshal(body, &req)
			killPID = req.Process.PID
			w.Write([]byte(`{}`))
		case "/process.Process/Update":
			var req ptyUpdateRequest
			json.Unmarshal(body, &req)
			resizePID = req.Process.PID
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	sb := newPtyTestSandbox(t, server)
	handle, err := sb.Pty().Create(context.Background(), PtySize{Rows: 24, Cols: 80}, PtyCreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := handle.Kill(context.Background()); err != nil {
		t.Fatalf("handle.Kill: %v", err)
	}
	if err := handle.Resize(context.Background(), PtySize{Rows: 40, Cols: 120}); err != nil {
		t.Fatalf("handle.Resize: %v", err)
	}
	if killPID != handle.PID() || resizePID != handle.PID() {
		t.Fatalf("delegated PIDs kill=%d resize=%d, want %d", killPID, resizePID, handle.PID())
	}
	if _, err := handle.Wait(nil); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

// TestPtyContextCancellation verifies cancelling the caller's context tears the
// stream down and unblocks Wait.
func TestPtyContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", connectContentType)
		w.Write(connectEnvelope(0, `{"event":{"start":{"pid":12}}}`))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer server.Close()

	sb := newPtyTestSandbox(t, server)
	ctx, cancel := context.WithCancel(context.Background())
	handle, err := sb.Pty().Create(ctx, PtySize{Rows: 24, Cols: 80}, PtyCreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	done := make(chan struct{})
	go func() {
		handle.Wait(nil)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Wait did not return after context cancellation")
	}
}
