// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func uploadProtocolSandbox(t *testing.T, handler http.Handler, h2 bool, timeout time.Duration, delayWrite bool) *Sandbox {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = h2
	scheme := "http"
	if h2 {
		server.StartTLS()
		scheme = "https"
	} else {
		server.Start()
	}
	t.Cleanup(server.Close)
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.ForceAttemptHTTP2 = h2
	if h2 {
		// Trust the httptest certificate while dialing its listener for the
		// SDK's sandbox hostname. Keep certificate verification enabled.
		transport.TLSClientConfig.ServerName = server.Listener.Addr().(*net.TCPAddr).IP.String()
	}
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		if err != nil {
			return nil, err
		}
		if delayWrite {
			return &uploadDelayedWriteConn{Conn: conn}, nil
		}
		return conn, nil
	}
	client := NewClient(Config{ProxyScheme: scheme, SandboxDomain: "cube.test"}, WithHTTPClient(&http.Client{
		Transport: transport, Timeout: timeout,
	}))
	t.Cleanup(func() { _ = client.Close() })
	return &Sandbox{client: client, SandboxID: "sb-upload-http"}
}

type uploadDelayedWriteConn struct {
	net.Conn
}

func (c *uploadDelayedWriteConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	// Bytes have reached the peer, but the writer has not resumed. This
	// lets HTTP/2 process the response before another request-body Read.
	time.Sleep(150 * time.Millisecond)
	return n, err
}

func TestFilesWriteFromCompleteHTTPBodyBeforeFinalEOFRead(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("http%d", map[bool]int{false: 1, true: 2}[h2]), func(t *testing.T) {
			checked := make(chan error, 1)
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if (r.ProtoMajor == 2) != h2 {
					t.Errorf("protocol=%s, h2=%v", r.Proto, h2)
				}
				reader, err := r.MultipartReader()
				if err == nil {
					part, partErr := reader.NextPart()
					err = partErr
					if err == nil {
						var data []byte
						data, err = io.ReadAll(part)
						if err == nil && string(data) != "content" {
							err = fmt.Errorf("uploaded=%q, want content", data)
						}
					}
					if err == nil {
						if _, endErr := reader.NextPart(); endErr != io.EOF {
							err = fmt.Errorf("multipart ending=%v, want EOF", endErr)
						}
					}
				}
				checked <- err
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				if !h2 {
					w.Header().Set("Connection", "close")
				}
				w.WriteHeader(http.StatusNoContent)
			})
			sb := uploadProtocolSandbox(t, handler, h2, 5*time.Second, true)
			if err := sb.Files().WriteFrom(context.Background(), "/tmp/file", strings.NewReader("content")); err != nil {
				t.Fatalf("complete upload failed: %v", err)
			}
			if err := <-checked; err != nil {
				t.Fatalf("server did not receive a complete multipart: %v", err)
			}
		})
	}
}

func TestFilesWriteFromIncompleteHTTPBodyWithEarlySuccess(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("http%d", map[bool]int{false: 1, true: 2}[h2]), func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if (r.ProtoMajor == 2) != h2 {
					t.Errorf("protocol=%s, h2=%v", r.Proto, h2)
				}
				if !h2 {
					if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
						t.Errorf("enable full duplex: %v", err)
					}
					w.Header().Set("Connection", "close")
				}
				_, _ = io.CopyN(io.Discard, r.Body, 1024)
				w.WriteHeader(http.StatusNoContent)
			})
			sb := uploadProtocolSandbox(t, handler, h2, 5*time.Second, false)
			err := sb.Files().WriteFrom(context.Background(), "/tmp/file", &uploadPatternReader{remaining: 8 << 20})
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("incomplete upload error=%v, want unexpected EOF", err)
			}
		})
	}
}

func TestFilesWriteFromEffectiveDeadlineAfterEarlySuccess(t *testing.T) {
	const short = 250 * time.Millisecond
	for _, h2 := range []bool{false, true} {
		for _, tc := range []struct {
			name    string
			client  time.Duration
			context time.Duration
		}{
			{name: "client_only", client: short},
			{name: "context_only", context: short},
			{name: "client_earlier", client: short, context: 5 * time.Second},
			{name: "context_earlier", client: 5 * time.Second, context: short},
		} {
			t.Run(fmt.Sprintf("http%d/%s", map[bool]int{false: 1, true: 2}[h2], tc.name), func(t *testing.T) {
				src := &uploadBlockedReader{entered: make(chan struct{}), release: make(chan struct{}), exited: make(chan struct{})}
				stop := make(chan struct{})
				responseSent := make(chan struct{})
				handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if (r.ProtoMajor == 2) != h2 {
						t.Errorf("protocol=%s, h2=%v", r.Proto, h2)
					}
					if !h2 {
						if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
							t.Errorf("enable full duplex: %v", err)
						}
					}
					select {
					case <-src.entered:
					case <-r.Context().Done():
						return
					}
					w.Header().Set("Content-Length", "2")
					_, _ = io.WriteString(w, "ok")
					w.(http.Flusher).Flush()
					close(responseSent)
					<-stop
				})
				sb := uploadProtocolSandbox(t, handler, h2, tc.client, false)
				t.Cleanup(func() {
					close(src.release)
					close(stop)
					select {
					case <-src.exited:
					case <-time.After(time.Second):
						t.Error("source Read did not exit after its owner unblocked it")
					}
				})
				ctx := context.Background()
				if tc.context > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, tc.context)
					defer cancel()
				}
				result := make(chan error, 1)
				go func() { result <- sb.Files().WriteFrom(ctx, "/tmp/file", src) }()
				select {
				case <-responseSent:
				case <-time.After(2 * time.Second):
					t.Fatal("server did not send the early successful response")
				}
				select {
				case err := <-result:
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("upload error=%v, want deadline exceeded", err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("upload outlived the effective request deadline")
				}
				if sb.client.dataHTTP.Timeout != tc.client {
					t.Fatal("WriteFrom changed the shared client's timeout")
				}
			})
		}
	}
}

type uploadBlockedReader struct {
	entered chan struct{}
	release chan struct{}
	exited  chan struct{}
	once    sync.Once
}

func (r *uploadBlockedReader) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.entered) })
	<-r.release
	close(r.exited)
	return 0, io.EOF
}
