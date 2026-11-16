// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"
)

func TestFilesWriteFromCompletionFallback(t *testing.T) {
	for _, pendingBody := range []bool{false, true} {
		t.Run(fmt.Sprintf("pending_body_%v", pendingBody), func(t *testing.T) {
			conn, peer := net.Pipe()
			defer conn.Close()
			defer peer.Close()
			client := NewClient(Config{RequestTimeout: 100 * time.Millisecond}, WithHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if !pendingBody {
					defer r.Body.Close()
					trace := httptrace.ContextClientTrace(r.Context())
					trace.GotConn(httptrace.GotConnInfo{Conn: conn})
					trace.WroteHeaders()
					if _, err := io.Copy(io.Discard, r.Body); err != nil {
						return nil, err
					}
				}
				// Omit the final trace callback (or leave body consumption pending).
				return &http.Response{StatusCode: 204, Header: make(http.Header), Body: http.NoBody}, nil
			})}))
			defer client.Close()
			result := make(chan error, 1)
			go func() {
				result <- (&Sandbox{client: client, SandboxID: "completion"}).Files().WriteFrom(context.Background(), "/tmp/file", strings.NewReader("content"))
			}()
			select {
			case err := <-result:
				if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "completion") {
					t.Fatalf("error=%v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("unbounded completion wait")
			}
			if client.dataHTTP.Timeout != 0 {
				t.Fatal("changed shared client timeout")
			}
		})
	}
}

func TestFilesWriteFromDeadlineBeforeResponse(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		for _, clientTimeout := range []bool{false, true} {
			t.Run(fmt.Sprintf("h2_%v/client_timeout_%v", h2, clientTimeout), func(t *testing.T) {
				src := &uploadBlockedReader{entered: make(chan struct{}), release: make(chan struct{}), exited: make(chan struct{})}
				stop := make(chan struct{})
				handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					select {
					case <-stop:
					case <-r.Context().Done():
					}
				})
				timeout := time.Duration(0)
				if clientTimeout {
					timeout = 300 * time.Millisecond
				}
				sb := uploadProtocolSandbox(t, handler, h2, timeout)
				t.Cleanup(func() {
					close(src.release)
					close(stop)
					select {
					case <-src.exited:
					case <-time.After(time.Second):
						t.Error("source did not exit")
					}
				})
				ctx := context.Background()
				if !clientTimeout {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, 300*time.Millisecond)
					defer cancel()
				}
				result := make(chan error, 1)
				go func() { result <- sb.Files().WriteFrom(ctx, "/tmp/file", src) }()
				select {
				case <-src.entered:
				case <-time.After(2 * time.Second):
					t.Fatal("source read did not start")
				}
				select {
				case err := <-result:
					var timeoutErr net.Error
					if !errors.Is(err, context.DeadlineExceeded) && !(errors.As(err, &timeoutErr) && timeoutErr.Timeout()) {
						t.Fatalf("error=%v", err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("blocked source pinned the caller past its deadline")
				}
				select {
				case <-src.exited:
					t.Fatal("caller returned only after source exited")
				default:
				}
			})
		}
	}
}

func TestMultipartUploadBodyCloseUnblocksRead(t *testing.T) {
	src := &uploadBlockedReader{entered: make(chan struct{}), release: make(chan struct{}), exited: make(chan struct{})}
	body, _, err := newMultipartUploadBody(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	defer func() {
		close(src.release)
		select {
		case <-body.workerDone:
		case <-time.After(time.Second):
			t.Error("source worker leaked after release")
		}
	}()
	result := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, body); result <- err }()
	select {
	case <-src.entered:
	case <-time.After(time.Second):
		t.Fatal("source read did not start")
	}
	body.Close()
	select {
	case err := <-result:
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock Read")
	}
}

func TestMultipartUploadBodyWorkerExitsAtEOF(t *testing.T) {
	body, _, err := newMultipartUploadBody(context.Background(), strings.NewReader("content"))
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	if _, err := io.Copy(io.Discard, body); err != nil {
		t.Fatal(err)
	}
	select {
	case <-body.workerDone:
	case <-time.After(time.Second):
		t.Fatal("worker did not exit at EOF")
	}
}

func TestUploadBlockedReaderRepeatedRead(t *testing.T) {
	r := &uploadBlockedReader{entered: make(chan struct{}), release: make(chan struct{}), exited: make(chan struct{})}
	close(r.release)
	for i := 0; i < 2; i++ {
		if _, err := r.Read(make([]byte, 1)); err != io.EOF {
			t.Fatal(err)
		}
	}
}

// A complete HTTP/2 request may be closed without a subsequent EOF-only read.
func TestMultipartFooterReportsEOFWithFinalBytes(t *testing.T) {
	r := &multipartFooterReader{reader: bytes.NewReader([]byte("footer"))}
	buf := make([]byte, 32)
	n, err := r.Read(buf)
	if n != 6 || err != io.EOF || string(buf[:n]) != "footer" {
		t.Fatalf("n=%d err=%v data=%q", n, err, buf[:n])
	}
}
