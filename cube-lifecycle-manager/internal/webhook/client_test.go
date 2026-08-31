// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package webhook

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

type recordedRequest struct {
	method      string
	contentType string
	signature   string
	body        []byte
}

// recordingServer is a configurable webhook receiver for tests. statuses is
// consumed front-to-back per request; once empty it falls back to 200.
type recordingServer struct {
	mu       sync.Mutex
	requests []recordedRequest
	statuses []int
	delay    time.Duration
}

func (s *recordingServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	body, _ := io.ReadAll(r.Body)

	s.mu.Lock()
	s.requests = append(s.requests, recordedRequest{
		method:      r.Method,
		contentType: r.Header.Get("Content-Type"),
		signature:   r.Header.Get(signatureHeader),
		body:        body,
	})
	status := http.StatusOK
	if len(s.statuses) > 0 {
		status = s.statuses[0]
		s.statuses = s.statuses[1:]
	}
	s.mu.Unlock()

	w.WriteHeader(status)
}

func (s *recordingServer) snapshot() []recordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]recordedRequest, len(s.requests))
	copy(out, s.requests)
	return out
}

func newTestServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func TestSend_PostsSignedBody(t *testing.T) {
	srv := newTestServer(t, &recordingServer{})
	ep := Endpoint{URL: srv.URL, Secret: "sekret", Enabled: true}
	ev := Event{Event: "sandbox.created", EventID: "stream-1", SandboxID: "sb-1"}

	c := NewClient(2*time.Second, 0, zap.NewNop())
	if err := c.Send(context.Background(), ep, ev); err != nil {
		t.Fatalf("Send: %v", err)
	}

	body, _ := ev.MarshalBody()
	wantSig := "sha256=" + signBody("sekret", body)

	got := srvRequests(t, srv, 1)[0]
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if got.contentType != "application/json" {
		t.Errorf("content-type = %q, want application/json", got.contentType)
	}
	if got.signature != wantSig {
		t.Errorf("signature = %q, want %q", got.signature, wantSig)
	}
	if !bytes.Equal(got.body, body) {
		t.Errorf("body mismatch\n got: %s\nwant: %s", got.body, body)
	}
}

func TestSend_NoSecretOmitsSignature(t *testing.T) {
	srv := newTestServer(t, &recordingServer{})
	ep := Endpoint{URL: srv.URL, Enabled: true}
	ev := Event{Event: "sandbox.paused", SandboxID: "sb-1"}

	c := NewClient(2*time.Second, 0, zap.NewNop())
	if err := c.Send(context.Background(), ep, ev); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got := srvRequests(t, srv, 1)[0]
	if got.signature != "" {
		t.Errorf("signature = %q, want empty when no secret", got.signature)
	}
}

func TestSend_RetriesThenSucceeds(t *testing.T) {
	rec := &recordingServer{statuses: []int{500, 500, 200}}
	srv := newTestServer(t, rec)
	ep := Endpoint{URL: srv.URL, Enabled: true}
	ev := Event{Event: "sandbox.created", SandboxID: "sb-1"}

	c := NewClient(2*time.Second, 2, zap.NewNop())
	start := time.Now()
	if err := c.Send(context.Background(), ep, ev); err != nil {
		t.Fatalf("Send after retries: %v", err)
	}
	if got := len(srvRequests(t, srv, 3)); got != 3 {
		t.Fatalf("requests = %d, want 3", got)
	}
	// 200ms + 400ms backoff with randomization=0; allow slack for CI timing.
	if time.Since(start) < 500*time.Millisecond {
		t.Error("retries happened too fast; backoff not applied")
	}
}

func TestSend_ExhaustsRetries(t *testing.T) {
	rec := &recordingServer{statuses: []int{500, 500, 500}}
	srv := newTestServer(t, rec)
	ep := Endpoint{URL: srv.URL, Enabled: true}
	ev := Event{Event: "sandbox.created", SandboxID: "sb-1"}

	c := NewClient(2*time.Second, 2, zap.NewNop())
	if err := c.Send(context.Background(), ep, ev); err == nil {
		t.Fatal("Send = nil, want error after exhausting retries")
	}
	if got := len(srvRequests(t, srv, 3)); got != 3 {
		t.Fatalf("requests = %d, want 3", got)
	}
}

func TestSend_TransportTimeout(t *testing.T) {
	rec := &recordingServer{delay: 200 * time.Millisecond}
	srv := newTestServer(t, rec)
	ep := Endpoint{URL: srv.URL, Enabled: true}
	ev := Event{Event: "sandbox.created", SandboxID: "sb-1"}

	c := NewClient(50*time.Millisecond, 0, zap.NewNop())
	if err := c.Send(context.Background(), ep, ev); err == nil {
		t.Fatal("Send = nil, want timeout error")
	}
}

func TestSend_AbortsOnCtxCancelDuringBackoff(t *testing.T) {
	rec := &recordingServer{statuses: []int{500, 500, 500}}
	srv := newTestServer(t, rec)
	ep := Endpoint{URL: srv.URL, Enabled: true}
	ev := Event{Event: "sandbox.created", SandboxID: "sb-1"}

	c := NewClient(2*time.Second, 2, zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel shortly after the first attempt fails, while the for-loop sleeps
	// in its exponential backoff — Send must abort promptly, not retry.
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()

	start := time.Now()
	err := c.Send(ctx, ep, ev)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Send = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Errorf("Send took %v, want prompt abort on ctx cancel", elapsed)
	}
	if n := len(rec.snapshot()); n != 1 {
		t.Fatalf("server saw %d requests, want 1 (cancel must stop retries)", n)
	}
}

// srvRequests waits until the server has recorded at least n requests.
func srvRequests(t *testing.T, srv *httptest.Server, n int) []recordedRequest {
	t.Helper()
	rec := srv.Config.Handler.(*recordingServer)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		reqs := rec.snapshot()
		if len(reqs) >= n {
			return reqs
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server recorded %d requests, want %d", len(rec.snapshot()), n)
	return nil
}
