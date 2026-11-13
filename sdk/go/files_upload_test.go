// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func uploadTestSandbox(t testing.TB, server *httptest.Server) *Sandbox {
	t.Helper()
	addr := server.Listener.Addr().(*net.TCPAddr)
	client := NewClient(Config{
		ProxyNodeIP: addr.IP.String(), ProxyPortHTTP: addr.Port, SandboxDomain: "cube.test",
	})
	t.Cleanup(func() { _ = client.Close() })
	return &Sandbox{
		client: client, SandboxID: "sb-upload",
		EnvdAccessToken: "envd-token", TrafficAccessToken: "traffic-token",
	}
}

func TestFilesWriteFromMultipartAndIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		user string
		path string
		data []byte
	}{
		{name: "empty"},
		{name: "binary", data: []byte{0, 1, 127, 128, 255, '\r', '\n'}},
		{name: "scoped", user: "app", data: []byte("contents")},
		{name: "newline_path", path: "/tmp/line\nbreak.bin", data: []byte("contents")},
		{name: "crlf_path", path: "/tmp/line\r\nbreak.bin", data: []byte("contents")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.path
			if path == "" {
				path = "/tmp/项目/quoted\" file.bin"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/files" || r.URL.Query().Get("path") != path {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
				}
				if r.URL.Query().Get("username") != tc.user || r.Header.Get("Authorization") != "" {
					t.Errorf("identity: username=%q authorization=%q", r.URL.Query().Get("username"), r.Header.Get("Authorization"))
				}
				if r.Header.Get("X-Access-Token") != "envd-token" || r.Header.Get("e2b-traffic-access-token") != "traffic-token" || r.Header.Get("cube-traffic-access-token") != "traffic-token" {
					t.Error("upload did not preserve envd/traffic authentication")
				}
				if r.ContentLength != -1 || len(r.TransferEncoding) != 1 || r.TransferEncoding[0] != "chunked" {
					t.Errorf("expected unknown-length streaming request, length=%d encoding=%v", r.ContentLength, r.TransferEncoding)
				}
				reader, err := r.MultipartReader()
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				part, err := reader.NextPart()
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				_, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
				if err != nil || part.FormName() != "file" || params["filename"] != "upload" {
					t.Errorf("part disposition=%v err=%v", params, err)
				}
				got, err := io.ReadAll(part)
				if err != nil || !bytes.Equal(got, tc.data) {
					t.Errorf("uploaded=%x, want=%x, err=%v", got, tc.data, err)
				}
				if _, err := reader.NextPart(); err != io.EOF {
					t.Errorf("multipart ending=%v, want EOF", err)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			sb := uploadTestSandbox(t, server)
			src := &uploadOwnedReader{Reader: bytes.NewReader(tc.data)}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := sb.Files().ForUser(tc.user).WriteFrom(ctx, path, src); err != nil {
				t.Fatal(err)
			}
			if src.closed.Load() {
				t.Fatal("WriteFrom closed the caller's reader")
			}
		})
	}
}

func TestFilesWriteFromStreamsBeforeSourceEOF(t *testing.T) {
	const total = 8 << 20
	const prefix = 64 << 10
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	received := make(chan struct{})
	result := make(chan [sha256.Size]byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reader, err := r.MultipartReader()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		part, err := reader.NextPart()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		hash := sha256.New()
		if _, err := io.CopyN(hash, part, prefix); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// The remaining source cannot be read until this prefix has arrived
		// over HTTP. An implementation that buffers the source first deadlocks.
		close(received)
		n, err := io.Copy(hash, part)
		if err != nil || n != total-prefix {
			http.Error(w, fmt.Sprintf("remaining=%d err=%v", n, err), http.StatusBadRequest)
			return
		}
		if _, err := reader.NextPart(); err != io.EOF {
			http.Error(w, "invalid multipart ending", http.StatusBadRequest)
			return
		}
		var sum [sha256.Size]byte
		copy(sum[:], hash.Sum(nil))
		result <- sum
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	sb := uploadTestSandbox(t, server)
	src := io.MultiReader(
		&uploadPatternReader{remaining: prefix},
		&uploadGatedReader{ctx: ctx, ready: received, reader: &uploadPatternReader{offset: prefix, remaining: total - prefix}},
	)
	if err := sb.Files().WriteFrom(ctx, "/tmp/large.bin", src); err != nil {
		t.Fatal(err)
	}
	expected := sha256.New()
	_, _ = io.Copy(expected, &uploadPatternReader{remaining: total})
	got := <-result
	if !bytes.Equal(got[:], expected.Sum(nil)) {
		t.Fatal("streamed upload changed the binary content")
	}
}

func TestFilesWriteFromDoesNotRetryOrFollowRedirects(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308, 400, 401, 413, 415, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Connection", "close")
				w.Header().Set("Location", "/redirect-target")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = fmt.Fprint(w, `{"message":"upload rejected"}`)
			}))
			defer server.Close()
			sb := uploadTestSandbox(t, server)
			var redirectCalls atomic.Int32
			sb.client.dataHTTP.CheckRedirect = func(*http.Request, []*http.Request) error {
				redirectCalls.Add(1)
				return nil
			}
			src := &uploadOwnedReader{Reader: &uploadPatternReader{remaining: 8 << 20}}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := sb.Files().WriteFrom(ctx, "/tmp/rejected.bin", src)
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", status)) || !strings.Contains(err.Error(), "upload rejected") {
				t.Fatalf("error=%v, want original HTTP rejection", err)
			}
			if calls.Load() != 1 || redirectCalls.Load() != 0 {
				t.Fatalf("requests=%d redirect callbacks=%d", calls.Load(), redirectCalls.Load())
			}
			if src.closed.Load() {
				t.Fatal("rejected upload closed the caller's reader")
			}
			// The one-shot policy must not replace the shared client's callback.
			_ = sb.client.dataHTTP.CheckRedirect(nil, nil)
			if redirectCalls.Load() != 1 {
				t.Fatal("WriteFrom modified the shared HTTP client")
			}
		})
	}
}

func TestFilesWriteFromReaderErrors(t *testing.T) {
	boom := errors.New("source failed")
	for _, data := range []string{"", "partial bytes"} {
		t.Run(fmt.Sprintf("bytes_%d", len(data)), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
			}))
			defer server.Close()
			sb := uploadTestSandbox(t, server)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := sb.Files().WriteFrom(ctx, "/tmp/failure", &uploadErrorReader{data: []byte(data), err: boom})
			if !errors.Is(err, boom) {
				t.Fatalf("error=%v, want source error", err)
			}
		})
	}
}

func TestFilesWriteFromRejectsIncompleteSuccess(t *testing.T) {
	boom := errors.New("source failed")
	for _, tc := range []struct {
		name    string
		consume bool
		want    error
	}{
		{name: "unread_body", want: io.ErrUnexpectedEOF},
		{name: "ignored_reader_error", consume: true, want: boom},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := NewClient(Config{}, WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if tc.consume {
					_, _ = io.Copy(io.Discard, req.Body)
				}
				_ = req.Body.Close()
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
			})}))
			defer client.Close()
			sb := &Sandbox{client: client, SandboxID: "sb-upload"}
			err := sb.Files().WriteFrom(context.Background(), "/tmp/file", &uploadErrorReader{err: boom})
			if !errors.Is(err, tc.want) {
				t.Fatalf("error=%v, want %v", err, tc.want)
			}
		})
	}
}

func TestFilesWriteFromCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buffer := make([]byte, 4096)
		if _, err := io.ReadFull(r.Body, buffer); err == nil {
			cancel()
		}
		_, _ = io.Copy(io.Discard, r.Body)
	}))
	defer server.Close()
	sb := uploadTestSandbox(t, server)
	src := &uploadOwnedReader{Reader: io.MultiReader(
		&uploadPatternReader{remaining: 64 << 10},
		&uploadGatedReader{ctx: ctx, ready: make(chan struct{}), reader: strings.NewReader("unreachable")},
	)}
	err := sb.Files().WriteFrom(ctx, "/tmp/cancelled", src)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want cancellation", err)
	}
	if src.closed.Load() {
		t.Fatal("cancelled upload closed the caller's reader")
	}
}

func TestFilesWriteFromValidationAndTransportErrors(t *testing.T) {
	for _, files := range []*Files{nil, {}, (&Sandbox{}).Files()} {
		if err := files.WriteFrom(context.Background(), "/tmp/file", strings.NewReader("content")); err == nil {
			t.Fatal("unattached Files must fail")
		}
	}
	boom := errors.New("transport failed")
	var calls atomic.Int32
	client := NewClient(Config{}, WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		_ = req.Body.Close()
		return nil, boom
	})}))
	defer client.Close()
	files := (&Sandbox{client: client, SandboxID: "sb-upload"}).Files()
	if err := files.WriteFrom(context.Background(), "/tmp/file", nil); err == nil {
		t.Fatal("nil source must fail")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := files.WriteFrom(ctx, "/tmp/file", &uploadErrorReader{err: boom}); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled upload error=%v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("invalid or pre-cancelled upload made an HTTP request")
	}
	if err := files.WriteFrom(context.Background(), "/tmp/file", strings.NewReader("content")); !errors.Is(err, boom) {
		t.Fatalf("transport error=%v", err)
	}
}

func BenchmarkFilesWriteFrom(b *testing.B) {
	for _, size := range []int64{64 << 10, 8 << 20} {
		b.Run(fmt.Sprintf("%dKiB", size>>10), func(b *testing.B) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reader, err := r.MultipartReader()
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				part, err := reader.NextPart()
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				if n, err := io.Copy(io.Discard, part); err != nil || n != size {
					http.Error(w, "incomplete upload", http.StatusBadRequest)
					return
				}
				if _, err := reader.NextPart(); err != io.EOF {
					http.Error(w, "invalid multipart ending", http.StatusBadRequest)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			sb := uploadTestSandbox(b, server)
			b.SetBytes(size)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := sb.Files().WriteFrom(context.Background(), "/tmp/benchmark", &uploadPatternReader{remaining: size}); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
		})
	}
}

type uploadOwnedReader struct {
	io.Reader
	closed atomic.Bool
}

func (r *uploadOwnedReader) Close() error {
	r.closed.Store(true)
	return nil
}

type uploadPatternReader struct {
	offset    int64
	remaining int64
}

func (r *uploadPatternReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := len(p)
	if int64(n) > r.remaining {
		n = int(r.remaining)
	}
	for i := 0; i < n; i++ {
		p[i] = byte((r.offset + int64(i)) % 251)
	}
	r.offset += int64(n)
	r.remaining -= int64(n)
	return n, nil
}

type uploadGatedReader struct {
	ctx    context.Context
	ready  <-chan struct{}
	reader io.Reader
}

func (r *uploadGatedReader) Read(p []byte) (int, error) {
	select {
	case <-r.ready:
		return r.reader.Read(p)
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	}
}

type uploadErrorReader struct {
	data []byte
	err  error
}

func (r *uploadErrorReader) Read(p []byte) (int, error) {
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, r.err
}
