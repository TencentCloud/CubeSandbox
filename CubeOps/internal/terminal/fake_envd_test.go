// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package terminal

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeEnvd is a stand-in for envd's process.Process API. It speaks just enough
// of the Connect protocol for the terminal client: a streaming Start/Connect
// that emits framed events, and unary control endpoints.
type fakeEnvd struct {
	srv *httptest.Server

	mu sync.Mutex
	// inputs records every SendInput payload (decoded).
	inputs []string
	// resizes records every Update size as "rowsxcols".
	resizes []string
	// signals counts SendSignal calls.
	signals int
	// hosts records the Host header of every request, so tests can assert
	// the <port>-<sandboxID>.<domain> routing.
	hosts []string

	// emit is called with the stream writer once Start/Connect is invoked.
	// It controls what the fake PTY produces. Nil means: start event only,
	// then block until the request context is cancelled.
	emit func(w *streamWriter)

	// startStatus, when non-zero, makes Start fail with that HTTP status.
	startStatus int
}

// streamWriter writes Connect stream frames to an HTTP response, flushing
// each one so the client sees it immediately.
type streamWriter struct {
	w    http.ResponseWriter
	f    http.Flusher
	done <-chan struct{}
}

func (s *streamWriter) frame(flags byte, payload []byte) {
	header := make([]byte, 5)
	header[0] = flags
	binary.BigEndian.PutUint32(header[1:5], uint32(len(payload)))
	_, _ = s.w.Write(header)
	_, _ = s.w.Write(payload)
	s.f.Flush()
}

func (s *streamWriter) event(v map[string]interface{}) {
	payload, _ := json.Marshal(map[string]interface{}{"event": v})
	s.frame(0, payload)
}

func (s *streamWriter) start(pid int) {
	s.event(map[string]interface{}{"start": map[string]interface{}{"pid": pid}})
}

func (s *streamWriter) data(raw string) {
	s.event(map[string]interface{}{
		"data": map[string]interface{}{"pty": base64.StdEncoding.EncodeToString([]byte(raw))},
	})
}

func (s *streamWriter) end(exitCode int) {
	s.event(map[string]interface{}{
		"end": map[string]interface{}{"exitCode": exitCode, "exited": true},
	})
	s.frame(connectEndStreamFlag, []byte("{}"))
}

// blockUntilDone keeps the stream open until the client goes away.
func (s *streamWriter) blockUntilDone() { <-s.done }

func newFakeEnvd(t *testing.T) *fakeEnvd {
	t.Helper()
	f := &fakeEnvd{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.hosts = append(f.hosts, r.Host)
		f.mu.Unlock()

		method := strings.TrimPrefix(r.URL.Path, "/process.Process/")
		switch method {
		case "Start", "Connect":
			if f.startStatus != 0 {
				w.WriteHeader(f.startStatus)
				_, _ = w.Write([]byte(`{"code":"internal","message":"boom"}`))
				return
			}
			flusher, ok := w.(http.Flusher)
			if !ok {
				t.Errorf("response writer is not a Flusher")
				return
			}
			w.Header().Set("Content-Type", connectJSONType)
			w.WriteHeader(http.StatusOK)
			sw := &streamWriter{w: w, f: flusher, done: r.Context().Done()}
			if f.emit != nil {
				f.emit(sw)
			} else {
				sw.start(4242)
				sw.blockUntilDone()
			}
		case "SendInput":
			body, _ := io.ReadAll(r.Body)
			var req struct {
				Input struct {
					PTY string `json:"pty"`
				} `json:"input"`
			}
			_ = json.Unmarshal(body, &req)
			raw, _ := base64.StdEncoding.DecodeString(req.Input.PTY)
			f.mu.Lock()
			f.inputs = append(f.inputs, string(raw))
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case "Update":
			body, _ := io.ReadAll(r.Body)
			var req struct {
				PTY struct {
					Size struct {
						Rows int `json:"rows"`
						Cols int `json:"cols"`
					} `json:"size"`
				} `json:"pty"`
			}
			_ = json.Unmarshal(body, &req)
			f.mu.Lock()
			f.resizes = append(f.resizes, itoa(req.PTY.Size.Rows)+"x"+itoa(req.PTY.Size.Cols))
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case "SendSignal":
			f.mu.Lock()
			f.signals++
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	// The client reads the proxy URL from the environment, exactly as it does
	// in production (AGENTHUB_SANDBOX_PROXY_URL points at cube-proxy).
	t.Setenv("AGENTHUB_SANDBOX_PROXY_URL", f.srv.URL)
	return f
}

func (f *fakeEnvd) snapshot() (inputs, resizes, hosts []string, signals int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.inputs...), append([]string(nil), f.resizes...),
		append([]string(nil), f.hosts...), f.signals
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// wrapCapture records the content-type and body of the next request before
// delegating to the wrapped handler.
func wrapCapture(next http.Handler, contentType, body *string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*contentType = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		*body = string(raw)
		r.Body = io.NopCloser(strings.NewReader(*body))
		next.ServeHTTP(w, r)
	})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
