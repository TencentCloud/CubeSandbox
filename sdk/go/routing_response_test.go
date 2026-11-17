// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestReadErrorMessageBoundsResponse(t *testing.T) {
	body := &routingBodySpy{ReadCloser: io.NopCloser(strings.NewReader(strings.Repeat("x", 2<<20)))}
	resp := &http.Response{StatusCode: http.StatusBadGateway, Body: body}
	message := readErrorMessage(resp)
	if read := body.bytes.Load(); read > 64<<10 {
		t.Fatalf("read %d bytes from error body, want at most 64 KiB", read)
	}
	if message == "" || len(message) > 64<<10 {
		t.Fatalf("error message length=%d, want a bounded nonempty prefix", len(message))
	}
}

func TestFilesListLargeSuccessResponse(t *testing.T) {
	// A diagnostic read limit must not truncate a valid directory listing.
	name := strings.Repeat("x", 100)
	listing := make([]map[string]string, 1000)
	for i := range listing {
		listing[i] = map[string]string{"name": fmt.Sprintf("%s-%d", name, i)}
	}
	response, err := json.Marshal(map[string]any{
		"entries": listing,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(response)
	}))
	defer server.Close()
	host, port := serverHostPort(t, server.URL)
	client := NewClient(Config{ProxyNodeIP: host, ProxyPortHTTP: port, SandboxDomain: "cube.test", RequestTimeout: time.Second})
	defer client.Close()
	sb := &Sandbox{client: client, SandboxID: "sb-list"}
	entries, err := sb.Files().List(context.Background(), "/")
	if err != nil || len(entries) != 1000 || entries[999].Name != name+"-999" {
		t.Fatalf("large listing was truncated: entries=%d err=%v", len(entries), err)
	}
}

type routingBodySpy struct {
	io.ReadCloser
	closes atomic.Int64
	bytes  atomic.Int64
}

func (b *routingBodySpy) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.bytes.Add(int64(n))
	return n, err
}

func (b *routingBodySpy) Close() error {
	b.closes.Add(1)
	return b.ReadCloser.Close()
}

type routingTrackingTransport struct {
	base   http.RoundTripper
	bodies []*routingBodySpy
}

func (r *routingTrackingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.base.RoundTrip(req)
	if err == nil {
		body := &routingBodySpy{ReadCloser: resp.Body}
		r.bodies = append(r.bodies, body)
		resp.Body = body
	}
	return resp, err
}

func TestRoutingResponseBodyLifecycle(t *testing.T) {
	for _, call := range []struct {
		name string
		run  func(*Sandbox) error
	}{
		{"Commands.Run", func(s *Sandbox) error {
			_, err := s.Commands().Run(context.Background(), "echo test", CommandOptions{})
			return err
		}},
		{"Pty.Kill", func(s *Sandbox) error { _, err := s.Pty().Kill(context.Background(), 1); return err }},
		{"Files.Stat", func(s *Sandbox) error { _, err := s.Files().Stat(context.Background(), "/x"); return err }},
		{"Files.Rename", func(s *Sandbox) error { _, err := s.Files().Rename(context.Background(), "/x", "/y"); return err }},
		{"Files.MakeDir", func(s *Sandbox) error { _, err := s.Files().MakeDir(context.Background(), "/x"); return err }},
		{"Files.List", func(s *Sandbox) error { _, err := s.Files().List(context.Background(), "/x"); return err }},
		{"Files.Remove", func(s *Sandbox) error { return s.Files().Remove(context.Background(), "/x") }},
	} {
		for _, size := range []int{128, 2 << 20} {
			for _, status := range []int{200, 404, 502} {
				t.Run(fmt.Sprintf("%s/%d/%d", call.name, size, status), func(t *testing.T) {
					payload := "<html>" + strings.Repeat("x", size-13) + "</html>"
					var mu sync.Mutex
					states := make(map[net.Conn]http.ConnState)
					accepted, closed := 0, 0
					server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "text/html")
						w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
						w.WriteHeader(status)
						io.WriteString(w, payload)
					}))
					server.Config.ConnState = func(c net.Conn, state http.ConnState) {
						mu.Lock()
						defer mu.Unlock()
						if state == http.StateNew {
							accepted++
						}
						if state == http.StateClosed {
							closed++
							delete(states, c)
						} else {
							states[c] = state
						}
					}
					server.Start()
					defer server.Close()
					host, port := serverHostPort(t, server.URL)
					client := NewClient(Config{ProxyNodeIP: host, ProxyPortHTTP: port, SandboxDomain: "cube.test", RequestTimeout: time.Second})
					transport := client.dataHTTP.Transport.(*http.Transport)
					tracker := &routingTrackingTransport{base: transport}
					client.dataHTTP.Transport = tracker
					client.dataHTTP.Timeout = 5 * time.Second
					defer transport.CloseIdleConnections()
					defer func() {
						for _, body := range tracker.bodies {
							body.ReadCloser.Close()
						}
					}()
					sb := &Sandbox{client: client, SandboxID: "sb-review"}
					for i := 0; i < 5; i++ {
						err := call.run(sb)
						var apiErr *APIError
						if !errors.As(err, &apiErr) || !strings.Contains(err.Error(), "HTML") {
							t.Fatalf("expected HTML routing APIError, got %v", err)
						}
						if errors.Is(err, ErrSandboxNotFound) {
							t.Fatalf("routing error classified as missing sandbox: %v", err)
						}
						if len(tracker.bodies) != i+1 {
							t.Fatalf("requests=%d, want %d", len(tracker.bodies), i+1)
						}
						if status >= 400 && tracker.bodies[i].bytes.Load() > 64<<10 {
							t.Fatalf("error response read %d bytes, want at most 64 KiB", tracker.bodies[i].bytes.Load())
						}
						if count := tracker.bodies[i].closes.Load(); count != 1 {
							t.Fatalf("request %d: original body closed %d times, want 1", i+1, count)
						}
					}
					// Allow server-side state callbacks to catch up before inspecting
					// active sockets. Idle connections remain eligible for reuse.
					deadline := time.Now().Add(3 * time.Second)
					for {
						mu.Lock()
						active := 0
						for _, state := range states {
							if state != http.StateIdle {
								active++
							}
						}
						currentAccepted, currentClosed, idle := accepted, closed, len(states)
						mu.Unlock()
						if active == 0 {
							t.Logf("requests=5 accepted=%d closed=%d idle=%d bodyCloses=5 firstBodyBytesRead=%d", currentAccepted, currentClosed, idle, tracker.bodies[0].bytes.Load())
							break
						}
						if time.Now().After(deadline) {
							t.Fatalf("%d connections still active", active)
						}
						time.Sleep(10 * time.Millisecond)
					}
					transport.CloseIdleConnections()
					deadline = time.Now().Add(3 * time.Second)
					for {
						mu.Lock()
						outstanding := accepted - closed
						mu.Unlock()
						if outstanding == 0 {
							break
						}
						if time.Now().After(deadline) {
							t.Fatalf("%d connections remain after closing idle connections", outstanding)
						}
						time.Sleep(10 * time.Millisecond)
					}
				})
			}
		}
	}
}
