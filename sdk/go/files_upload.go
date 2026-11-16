// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
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
	ctx, cancelRequest := context.WithCancel(ctx)
	defer cancelRequest()
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
		return fmt.Errorf("failed to write %s: %w", path, err)
	}
	body, contentType, err := newMultipartUploadBody(ctx, src)
	if err != nil {
		return err
	}
	defer body.Close()

	writes := newUploadWriteObserver()
	requestCtx := httptrace.WithClientTrace(ctx, writes.trace())
	req, err := s.newEnvdRequest(requestCtx, http.MethodPost, envdFilesPath, newEnvdFileQuery(path, options...), body)
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
	// Bound completion even when the caller supplied no deadline. Start this
	// fallback after the response, so it does not limit a long-running upload.
	completionCtx := ctx
	if _, bounded := ctx.Deadline(); !bounded {
		var cancel context.CancelFunc
		completionCtx, cancel = context.WithTimeout(ctx, s.client.config.RequestTimeout)
		defer cancel()
	}
	select {
	case <-body.done:
		if err := completionCtx.Err(); err != nil {
			return fmt.Errorf("failed to write %s: %w", path, err)
		}
		if body.err != nil {
			return fmt.Errorf("failed to write %s: %w", path, body.err)
		}
		if err := writes.wait(completionCtx); err != nil {
			return fmt.Errorf("failed to write %s: %w", path, err)
		}
		return nil
	case <-completionCtx.Done():
		return fmt.Errorf("failed to write %s: upload completion timed out or was canceled: %w", path, completionCtx.Err())
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

// multipartUploadBody uses one demand-driven worker and a fixed buffer so Close
// can unblock the transport's Read without closing the caller's source. A source
// stuck inside Read can retain that worker until its owner unblocks it.
type multipartUploadBody struct {
	ctx        context.Context
	reader     io.Reader
	done       chan struct{}
	once       sync.Once
	err        error // published by closing done
	readMu     sync.Mutex
	start      sync.Once
	requests   chan int
	results    chan uploadReadResult
	workerDone chan struct{}
}

const uploadReadBufferSize = 32 << 10

type uploadReadResult struct {
	data []byte
	err  error
}

func newMultipartUploadBody(ctx context.Context, src io.Reader) (*multipartUploadBody, string, error) {
	// The destination is carried by the path query parameter. Keep it out
	// of MIME headers: older Go versions do not escape CR/LF in filenames.
	header, footer, contentType, err := multipartFileFraming(streamUploadFilename)
	if err != nil {
		return nil, "", err
	}
	return &multipartUploadBody{
		ctx: ctx,
		reader: io.MultiReader(
			bytes.NewReader(header),
			src,
			&multipartFooterReader{reader: bytes.NewReader(footer)},
		),
		done:       make(chan struct{}),
		requests:   make(chan int),
		results:    make(chan uploadReadResult),
		workerDone: make(chan struct{}),
	}, contentType, nil
}

func (b *multipartUploadBody) Read(p []byte) (int, error) {
	b.readMu.Lock()
	defer b.readMu.Unlock()
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
	if len(p) == 0 {
		return 0, nil
	}
	b.start.Do(func() { go b.readSource() })
	select {
	case b.requests <- min(len(p), uploadReadBufferSize):
	case <-b.ctx.Done():
		b.finish(b.ctx.Err())
	case <-b.done:
	}
	select {
	case result := <-b.results:
		n := copy(p, result.data)
		if result.err == io.EOF {
			b.finish(nil)
		} else if result.err != nil {
			b.finish(result.err)
		}
		return n, result.err
	case <-b.ctx.Done():
		b.finish(b.ctx.Err())
	case <-b.done:
	}
	<-b.done
	if b.err != nil {
		return 0, b.err
	}
	return 0, io.EOF
}

func (b *multipartUploadBody) readSource() {
	defer close(b.workerDone)
	buffer := make([]byte, uploadReadBufferSize)
	for {
		var size int
		select {
		case <-b.done:
			return
		case <-b.ctx.Done():
			return
		case size = <-b.requests:
		}
		n, err := b.reader.Read(buffer[:size])
		select {
		case <-b.done:
			return
		case <-b.ctx.Done():
			return
		case b.results <- uploadReadResult{data: buffer[:n], err: err}:
		}
		if err != nil {
			return
		}
	}
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
