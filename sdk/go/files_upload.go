// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"time"
)

func (s *Sandbox) writeFileFrom(ctx context.Context, path string, src io.Reader, options ...fileRequestOption) error {
	if err := s.ensureClient(); err != nil {
		return err
	}
	if src == nil {
		return fmt.Errorf("file source must not be nil")
	}
	httpClient := *s.client.dataHTTP
	if httpClient.Timeout > 0 {
		// Include the post-response completion wait in the client's timeout.
		// Keep Client.Timeout as well: http.Client uses it to adapt legacy
		// transports that depend on Request.Cancel or CancelRequest.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, httpClient.Timeout)
		defer cancel()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	body, contentType, err := newMultipartUploadBody(ctx, src)
	if err != nil {
		return err
	}
	defer body.Close()

	writes := newUploadWriteObserver()
	requestCtx := httptrace.WithClientTrace(ctx, writes.trace())
	req, err := s.newEnvdRequest(requestCtx, http.MethodPost, "/files", newEnvdFileQuery(path, options...), body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)

	// A redirect can turn a POST into a GET, or require replaying src. Keep
	// this one-shot policy local, without changing a shared client's settings.
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	if deadline, ok := ctx.Deadline(); ok {
		// The legacy cancellation adapter also needs the earlier caller
		// deadline; a transport may not observe Request.Context itself.
		httpClient.Timeout = time.Until(deadline)
		if httpClient.Timeout <= 0 {
			return fmt.Errorf("failed to write %s: %w", path, context.DeadlineExceeded)
		}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("failed to write %s: %w", path, ctx.Err())
		}
		return fmt.Errorf("failed to write %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		message := readErrorMessage(resp)
		if message == "" {
			message = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("failed to write %s: HTTP %d: %s", path, resp.StatusCode, message)
	}

	// A server can reply before the request body has finished. Do not turn
	// a source read failure or a prematurely closed upload into success.
	select {
	case <-body.done:
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("failed to write %s: %w", path, err)
		}
		if body.err != nil {
			return fmt.Errorf("failed to write %s: %w", path, body.err)
		}
		if err := writes.wait(ctx); err != nil {
			return fmt.Errorf("failed to write %s: %w", path, err)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("failed to write %s: %w", path, ctx.Err())
	}
}

// Source EOF means bytes were handed to the transport, not that its last
// network Write succeeded. Standard HTTP transports report that result through
// WroteRequest, possibly after Do has already returned an early response.
type uploadWriteObserver struct {
	conn    atomic.Bool
	headers atomic.Bool
	result  chan error
}

func newUploadWriteObserver() *uploadWriteObserver {
	return &uploadWriteObserver{result: make(chan error, 1)}
}

func (w *uploadWriteObserver) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			if info.Conn != nil {
				w.conn.Store(true)
			}
		},
		WroteHeaders: func() { w.headers.Store(true) },
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			select {
			case w.result <- info.Err:
			default:
			}
		},
	}
}

func (w *uploadWriteObserver) wait(ctx context.Context) error {
	select {
	case err := <-w.result:
		return err
	default:
	}
	if !w.conn.Load() || !w.headers.Load() {
		// Custom transports need not implement HTTP tracing. Their response
		// and write-error contract, including registered protocol handlers,
		// remains the caller's responsibility.
		return nil
	}
	// Observe the lifecycle even through an instrumentation wrapper. Testing
	// the RoundTripper's concrete type would silently skip the write result
	// of a wrapped standard transport. A transport that starts this lifecycle
	// must finish it; a grace period cannot prove a pending Write succeeded.
	select {
	case err := <-w.result:
		return err
	case <-ctx.Done():
		return fmt.Errorf("request-write completion was not reported: %w", ctx.Err())
	}
}

// multipartUploadBody buffers only the multipart framing. The transport reads
// src directly, so there is no producer goroutine or file-sized buffer. Closing
// the body stops future reads without taking ownership of src.
type multipartUploadBody struct {
	ctx    context.Context
	reader io.Reader
	done   chan struct{}
	once   sync.Once
	err    error // published by closing done
}

func newMultipartUploadBody(ctx context.Context, src io.Reader) (*multipartUploadBody, string, error) {
	var framing bytes.Buffer
	writer := multipart.NewWriter(&framing)
	// The destination is carried by the path query parameter. Keep it out
	// of MIME headers: older Go versions do not escape CR/LF in filenames.
	if _, err := writer.CreateFormFile("file", "upload"); err != nil {
		return nil, "", err
	}
	headerLen := framing.Len()
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	raw := framing.Bytes()
	return &multipartUploadBody{
		ctx: ctx,
		reader: io.MultiReader(
			bytes.NewReader(raw[:headerLen]),
			src,
			&multipartFooterReader{reader: bytes.NewReader(raw[headerLen:])},
		),
		done: make(chan struct{}),
	}, writer.FormDataContentType(), nil
}

func (b *multipartUploadBody) Read(p []byte) (int, error) {
	select {
	case <-b.done:
		if b.err != nil {
			return 0, b.err
		}
		return 0, io.EOF
	default:
	}
	if err := b.ctx.Err(); err != nil {
		b.finish(err)
		return 0, err
	}
	n, err := b.reader.Read(p)
	if err == io.EOF {
		b.finish(nil)
	} else if err != nil {
		b.finish(err)
	}
	return n, err
}

func (b *multipartUploadBody) Close() error {
	err := b.ctx.Err()
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	b.finish(err)
	return nil
}

func (b *multipartUploadBody) finish(err error) {
	b.once.Do(func() {
		b.err = err
		close(b.done)
	})
}

// Report EOF together with the last delimiter bytes. HTTP/2 can close the
// request after the server accepts those bytes, without requesting another
// Read. This records source completion; WroteRequest checks the network write.
type multipartFooterReader struct {
	reader *bytes.Reader
}

func (r *multipartFooterReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err == nil && r.reader.Len() == 0 {
		err = io.EOF
	}
	return n, err
}
