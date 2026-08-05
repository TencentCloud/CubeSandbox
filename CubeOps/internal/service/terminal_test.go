// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestTerminalClientSessionLifecycle(t *testing.T) {
	var mu sync.Mutex
	requests := make(map[string][]byte)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "49983-sb-123.test.cube.app" {
			t.Errorf("Host = %q", r.Host)
		}
		if r.Header.Get("Authorization") != terminalAuth {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests[r.URL.Path] = body
		mu.Unlock()

		if r.URL.Path != "/process.Process/Start" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		var start terminalStartRequest
		if len(body) < 5 || json.Unmarshal(body[5:], &start) != nil {
			t.Errorf("invalid Connect start body: %q", body)
		}
		if start.Process.Cmd != "/bin/bash" || start.Process.Cwd != "/root" {
			t.Errorf("unexpected process config: %#v", start.Process)
		}
		if start.PTY.Size != (TerminalSize{Rows: 24, Cols: 80}) {
			t.Errorf("start size = %#v", start.PTY.Size)
		}

		w.Header().Set("Content-Type", terminalConnectContentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(testTerminalEnvelope(t, terminalProcessResponse{Event: &terminalProcessEvent{Start: &terminalProcessStartEvent{PID: 42}}}))
		_, _ = w.Write(testTerminalEnvelope(t, terminalProcessResponse{Event: &terminalProcessEvent{Data: &terminalProcessDataEvent{PTY: base64.StdEncoding.EncodeToString([]byte("hello\r\n"))}}}))
		code := 0
		_, _ = w.Write(testTerminalEnvelope(t, terminalProcessResponse{Event: &terminalProcessEvent{End: &terminalProcessEndEvent{ExitCode: &code, Exited: true}}}))
	}))
	defer srv.Close()

	client := NewTerminalClientWithHTTP(srv.URL, "test.cube.app", srv.Client(), srv.Client())
	session, err := client.Open(context.Background(), "sb-123", TerminalSize{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if session.PID() != 42 {
		t.Fatalf("PID = %d, want 42", session.PID())
	}
	if err := session.SendInput(context.Background(), []byte("pwd\n")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	if err := session.Resize(context.Background(), TerminalSize{Rows: 40, Cols: 120}); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if err := session.Kill(context.Background()); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	var output []byte
	for chunk := range session.Output() {
		output = append(output, chunk...)
	}
	if string(output) != "hello\r\n" {
		t.Fatalf("output = %q", output)
	}
	exit := <-session.Done()
	if exit.Error != nil || exit.ExitCode == nil || *exit.ExitCode != 0 {
		t.Fatalf("exit = %#v", exit)
	}

	mu.Lock()
	defer mu.Unlock()
	var input terminalInputRequest
	if err := json.Unmarshal(requests["/process.Process/SendInput"], &input); err != nil {
		t.Fatalf("decode input request: %v", err)
	}
	if input.Process.PID != 42 || input.Input.PTY != base64.StdEncoding.EncodeToString([]byte("pwd\n")) {
		t.Fatalf("input request = %#v", input)
	}
	var resize terminalUpdateRequest
	if err := json.Unmarshal(requests["/process.Process/Update"], &resize); err != nil {
		t.Fatalf("decode resize request: %v", err)
	}
	if resize.Process.PID != 42 || resize.PTY.Size != (TerminalSize{Rows: 40, Cols: 120}) {
		t.Fatalf("resize request = %#v", resize)
	}
}

func TestTerminalClientErrorsAndBounds(t *testing.T) {
	if got := NormalizeTerminalSize(TerminalSize{Rows: -1, Cols: 900}); got != (TerminalSize{Rows: 24, Cols: 500}) {
		t.Fatalf("NormalizeTerminalSize = %#v", got)
	}
	client := NewTerminalClientWithHTTP("http://127.0.0.1", "", nil, nil)
	if _, err := client.Open(context.Background(), "sb", TerminalSize{}); err == nil {
		t.Fatal("Open accepted an empty sandbox domain")
	}
	if _, err := client.Open(context.Background(), "", TerminalSize{}); err == nil {
		t.Fatal("Open accepted an empty sandbox ID")
	}
	if _, err := client.Open(context.Background(), "../bad-host", TerminalSize{}); err == nil {
		t.Fatal("Open accepted an invalid sandbox ID")
	}

	var oversized [5]byte
	binary.BigEndian.PutUint32(oversized[1:], terminalMaxEnvelopeSize+1)
	if _, _, err := readTerminalEvent(bytes.NewReader(oversized[:])); err == nil {
		t.Fatal("readTerminalEvent accepted an oversized frame")
	}
	compressed := append([]byte{1, 0, 0, 0, 2}, []byte(`{}`)...)
	if _, _, err := readTerminalEvent(bytes.NewReader(compressed)); err == nil {
		t.Fatal("readTerminalEvent accepted a compressed frame")
	}
}

func TestTerminalKillTreatsNotFoundAsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"not_found"}`))
	}))
	defer srv.Close()
	client := NewTerminalClientWithHTTP(srv.URL, "test.cube.app", srv.Client(), &http.Client{Timeout: time.Second})
	if err := client.unary(context.Background(), "sb", "SendSignal", terminalSignalRequest{}, true); err != nil {
		t.Fatalf("not-found kill = %v", err)
	}
}

func testTerminalEnvelope(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encodeTerminalEnvelope(raw)
}
