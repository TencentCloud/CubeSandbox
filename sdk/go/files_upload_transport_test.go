// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFilesWriteFromRejectsFailedFooterWrite(t *testing.T) {
	for _, tc := range []struct {
		name    string
		h2      bool
		wrapped bool
	}{
		{name: "http1"},
		{name: "http2", h2: true},
		{name: "wrapped_http1", wrapped: true},
		{name: "wrapped_http2", h2: true, wrapped: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h2 := tc.h2
			const payload = "content"
			sourceEOF := make(chan struct{})
			footerAttempt := make(chan struct{})
			responseWritten := make(chan struct{})
			abort := make(chan struct{})
			packet := make(chan []byte, 1)
			boundary := make(chan string, 1)
			serverRead := make(chan error, 1)
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
				_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
				boundary <- params["boundary"]
				reader := bufio.NewReader(r.Body)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						serverRead <- err
						return
					}
					if line == "\r\n" {
						break
					}
				}
				data := make([]byte, len(payload))
				_, err := io.ReadFull(reader, data)
				if err == nil && string(data) != payload {
					err = fmt.Errorf("received=%q, want %q", data, payload)
				}
				serverRead <- err
				select {
				case <-footerAttempt:
				case <-abort:
					return
				}
				w.WriteHeader(http.StatusNoContent)
				w.(http.Flusher).Flush()
				close(responseWritten)
			})
			sb := uploadProtocolSandbox(t, handler, h2, 5*time.Second, false)
			t.Cleanup(func() { close(abort) })
			transport := sb.client.dataHTTP.Transport.(*http.Transport)
			dial := transport.DialContext
			transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				conn, err := dial(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				return &uploadFooterFailureConn{
					Conn: conn, sourceEOF: sourceEOF, attempted: footerAttempt,
					response: responseWritten, abort: abort, packet: packet,
				}, nil
			}
			if tc.wrapped {
				// Instrumentation wrappers delegate to the real transport and
				// must not disable its asynchronous request-write check.
				sb.client.dataHTTP.Transport = roundTripFunc(transport.RoundTrip)
			}
			traceResult := make(chan error, 1)
			ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
				WroteRequest: func(info httptrace.WroteRequestInfo) { traceResult <- info.Err },
			})
			src := &uploadEOFReader{reader: strings.NewReader(payload), eof: sourceEOF}
			err := sb.Files().WriteFrom(ctx, "/tmp/file", src)
			if err == nil {
				t.Fatal("accepted 2xx while the final multipart Write failed")
			}
			select {
			case readErr := <-serverRead:
				if readErr != nil {
					t.Fatalf("server did not receive the payload: %v", readErr)
				}
			case <-time.After(time.Second):
				t.Fatal("server did not report receiving the payload")
			}
			select {
			case dropped := <-packet:
				b := <-boundary
				if !h2 && !bytes.Contains(dropped, []byte("\r\n--"+b+"--\r\n")) {
					t.Fatalf("injected failure did not drop the multipart footer: %q", dropped)
				}
			case <-time.After(time.Second):
				t.Fatal("final Write was not intercepted")
			}
			select {
			case writeErr := <-traceResult:
				if writeErr == nil {
					t.Fatal("caller trace did not observe the write failure")
				}
				if !h2 && !errors.Is(err, writeErr) {
					t.Fatalf("upload error=%v did not preserve trace error=%v", err, writeErr)
				}
			case <-time.After(time.Second):
				t.Fatal("SDK did not preserve the caller's WroteRequest hook")
			}
		})
	}
}

type uploadEOFReader struct {
	reader io.Reader
	eof    chan struct{}
	once   sync.Once
}

func (r *uploadEOFReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err == io.EOF {
		r.once.Do(func() { close(r.eof) })
	}
	return n, err
}

type uploadFooterFailureConn struct {
	net.Conn
	sourceEOF <-chan struct{}
	attempted chan struct{}
	response  <-chan struct{}
	abort     <-chan struct{}
	packet    chan []byte
	failed    atomic.Bool
}

func (c *uploadFooterFailureConn) Write(p []byte) (int, error) {
	select {
	case <-c.sourceEOF:
		if c.failed.CompareAndSwap(false, true) {
			c.packet <- append([]byte(nil), p...)
			close(c.attempted)
			select {
			case <-c.response:
			case <-c.abort:
			}
			// Let the real response reach the transport while the final
			// write is pending, then report that none of its bytes were sent.
			time.Sleep(150 * time.Millisecond)
		}
		return 0, io.ErrClosedPipe
	default:
		return c.Conn.Write(p)
	}
}

func TestFilesWriteFromLegacyTransportDeadlines(t *testing.T) {
	const short = 150 * time.Millisecond
	for _, requestCancel := range []bool{false, true} {
		for _, tc := range []struct {
			name    string
			client  time.Duration
			context time.Duration
		}{
			{name: "client_only", client: short},
			{name: "client_earlier", client: short, context: 5 * time.Second},
			{name: "context_earlier", client: 5 * time.Second, context: short},
			{name: "context_only", context: short},
		} {
			t.Run(fmt.Sprintf("request_cancel_%v/%s", requestCancel, tc.name), func(t *testing.T) {
				transport := &uploadLegacyTransport{release: make(chan struct{}), requestCancel: requestCancel}
				defer transport.unblock()
				shared := &http.Client{Transport: transport, Timeout: tc.client}
				client := NewClient(Config{}, WithHTTPClient(shared))
				defer client.Close()
				sb := &Sandbox{client: client, SandboxID: "legacy-upload"}
				ctx := context.Background()
				if tc.context > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, tc.context)
					defer cancel()
				}
				result := make(chan error, 1)
				go func() { result <- sb.Files().WriteFrom(ctx, "/tmp/file", strings.NewReader("content")) }()
				select {
				case err := <-result:
					var timeout net.Error
					// Go 1.22's Client.Timeout wrapper exposes Timeout but
					// does not always wrap context.DeadlineExceeded.
					if !errors.Is(err, context.DeadlineExceeded) && !(errors.As(err, &timeout) && timeout.Timeout()) {
						t.Fatalf("legacy transport error=%v, want a timeout", err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("legacy transport outlived the effective deadline")
				}
				if shared.Timeout != tc.client {
					t.Fatal("WriteFrom changed the shared client's timeout")
				}
			})
		}
	}
}

type uploadLegacyTransport struct {
	release       chan struct{}
	once          sync.Once
	requestCancel bool
}

func (t *uploadLegacyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	defer req.Body.Close()
	var cancelled <-chan struct{}
	if t.requestCancel {
		cancelled = req.Cancel
	}
	select {
	case <-cancelled:
	case <-t.release:
	}
	return nil, context.DeadlineExceeded
}

func (t *uploadLegacyTransport) CancelRequest(*http.Request) {
	if !t.requestCancel {
		t.unblock()
	}
}

func (t *uploadLegacyTransport) unblock() {
	t.once.Do(func() { close(t.release) })
}

func TestFilesWriteFromCustomTransportWithoutWriteTrace(t *testing.T) {
	custom := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		defer req.Body.Close()
		// A custom transport may emit only some trace hooks. It must not be
		// required to emit WroteRequest in order to return its own response.
		httptrace.ContextClientTrace(req.Context()).WroteHeaders()
		if _, err := io.Copy(io.Discard, req.Body); err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: http.NoBody}, nil
	})
	for _, registered := range []bool{false, true} {
		t.Run(fmt.Sprintf("registered_%v", registered), func(t *testing.T) {
			var transport http.RoundTripper = custom
			if registered {
				standard := &http.Transport{}
				standard.RegisterProtocol("http", custom)
				transport = standard
			}
			client := NewClient(Config{}, WithHTTPClient(&http.Client{Transport: transport}))
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := (&Sandbox{client: client, SandboxID: "custom-upload"}).Files().WriteFrom(ctx, "/tmp/file", strings.NewReader("content")); err != nil {
				t.Fatalf("custom transport depended on an optional trace hook: %v", err)
			}
		})
	}
}

func TestFilesWriteFromCustomTransportReportedWriteError(t *testing.T) {
	want := errors.New("custom transport failed to write the request")
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		defer req.Body.Close()
		if _, err := io.Copy(io.Discard, req.Body); err != nil {
			return nil, err
		}
		httptrace.ContextClientTrace(req.Context()).WroteRequest(httptrace.WroteRequestInfo{Err: want})
		return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: http.NoBody}, nil
	})
	client := NewClient(Config{}, WithHTTPClient(&http.Client{Transport: transport}))
	defer client.Close()
	err := (&Sandbox{client: client, SandboxID: "custom-upload"}).Files().WriteFrom(context.Background(), "/tmp/file", strings.NewReader("content"))
	if !errors.Is(err, want) {
		t.Fatalf("upload error=%v, want the reported write error", err)
	}
}

func TestFilesWriteFromPartialTraceHonorsDeadline(t *testing.T) {
	for _, registered := range []bool{false, true} {
		t.Run(fmt.Sprintf("registered_%v", registered), func(t *testing.T) {
			conn, peer := net.Pipe()
			defer conn.Close()
			defer peer.Close()
			custom := roundTripFunc(func(req *http.Request) (*http.Response, error) {
				defer req.Body.Close()
				trace := httptrace.ContextClientTrace(req.Context())
				trace.GotConn(httptrace.GotConnInfo{Conn: conn})
				trace.WroteHeaders()
				if _, err := io.Copy(io.Discard, req.Body); err != nil {
					return nil, err
				}
				// A missing final callback does not prove the write succeeded.
				return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: http.NoBody}, nil
			})
			var transport http.RoundTripper = custom
			if registered {
				standard := &http.Transport{}
				standard.RegisterProtocol("http", custom)
				transport = standard
			}
			client := NewClient(Config{}, WithHTTPClient(&http.Client{Transport: transport}))
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer cancel()
			result := make(chan error, 1)
			go func() {
				result <- (&Sandbox{client: client, SandboxID: "partial-trace-upload"}).Files().WriteFrom(ctx, "/tmp/file", strings.NewReader("content"))
			}()
			select {
			case err := <-result:
				if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "request-write completion") {
					t.Fatalf("upload error=%v, want an explicit completion deadline error", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("partial trace outlived the context deadline")
			}
		})
	}
}

func TestFilesWriteFromPrefersHTTPRejection(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("http%d", map[bool]int{false: 1, true: 2}[h2]), func(t *testing.T) {
			stop := make(chan struct{})
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !h2 {
					_ = http.NewResponseController(w).EnableFullDuplex()
				}
				w.Header().Set("Content-Length", "50")
				w.WriteHeader(http.StatusRequestEntityTooLarge)
				w.(http.Flusher).Flush()
				<-stop
			})
			sb := uploadProtocolSandbox(t, handler, h2, 250*time.Millisecond, false)
			t.Cleanup(func() { close(stop) })
			err := sb.Files().WriteFrom(context.Background(), "/tmp/file", strings.NewReader("content"))
			if err == nil || !strings.Contains(err.Error(), "HTTP 413") || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error=%v, want the HTTP rejection despite the stalled error body", err)
			}
		})
	}
}
