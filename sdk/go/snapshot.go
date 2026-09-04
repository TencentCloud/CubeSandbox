// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// SnapshotInfo is the metadata returned by snapshot APIs. Snapshots are stored
// as templates, so SnapshotID doubles as a template ID for create/delete.
type SnapshotInfo struct {
	SnapshotID string   `json:"snapshotID"`
	Names      []string `json:"names"`
}

// ListSnapshotsOptions filters the snapshot listing. Zero values are omitted.
type ListSnapshotsOptions struct {
	SandboxID string
	Limit     int
	NextToken string
}

// CloneOptions controls Sandbox.Clone. N defaults to 1; Concurrency defaults to
// 1 (sequential). Concurrency is capped at N.
type CloneOptions struct {
	N           int
	Concurrency int
}

type cloneCleanup struct {
	mu             sync.Mutex
	client         *Client
	snapshotID     string
	remaining      int
	released       map[string]struct{}
	cleanupStarted bool
}

func (c *cloneCleanup) release(ctx context.Context, sandboxID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if _, ok := c.released[sandboxID]; ok {
		c.mu.Unlock()
		return
	}
	c.released[sandboxID] = struct{}{}
	c.remaining--
	shouldCleanup := c.startCleanupLocked(c.remaining == 0)
	c.mu.Unlock()
	if shouldCleanup {
		_ = c.client.DeleteSnapshot(context.WithoutCancel(ctx), c.snapshotID)
	}
}

func (c *cloneCleanup) cleanup(ctx context.Context) {
	if c == nil {
		return
	}
	c.mu.Lock()
	shouldCleanup := c.startCleanupLocked(true)
	c.mu.Unlock()
	if shouldCleanup {
		_ = c.client.DeleteSnapshot(context.WithoutCancel(ctx), c.snapshotID)
	}
}

func (c *cloneCleanup) startCleanupLocked(ready bool) bool {
	if !ready || c.cleanupStarted {
		return false
	}
	c.cleanupStarted = true
	return true
}

// CreateSnapshot captures the current sandbox state (POST
// /sandboxes/:id/snapshots). The snapshot outlives the sandbox. An empty name
// lets the server pick one; a known name attaches a new build to it.
func (s *Sandbox) CreateSnapshot(ctx context.Context, name string) (*SnapshotInfo, error) {
	if err := s.ensureClient(); err != nil {
		return nil, err
	}
	// Always send a JSON object body: the server deserializes into a
	// CreateSnapshotRequest struct and rejects an empty/null body with 422,
	// even though every field is optional. An empty name simply omits it.
	payload := map[string]any{}
	if name != "" {
		payload["name"] = name
	}
	var info SnapshotInfo
	path := "/sandboxes/" + url.PathEscape(s.SandboxID) + "/snapshots"
	if err := s.client.doJSON(ctx, http.MethodPost, path, payload, &info, http.StatusOK, http.StatusCreated); err != nil {
		return nil, err
	}
	return &info, nil
}

// ListSnapshots pages through snapshots (GET /snapshots). It returns the page
// items plus the next-page token (empty when there are no more pages).
func (c *Client) ListSnapshots(ctx context.Context, opts ListSnapshotsOptions) ([]SnapshotInfo, string, error) {
	query := url.Values{}
	if opts.SandboxID != "" {
		query.Set("sandboxID", opts.SandboxID)
	}
	if opts.Limit > 0 {
		query.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.NextToken != "" {
		query.Set("nextToken", opts.NextToken)
	}
	path := "/snapshots"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}

	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.controlHTTP.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", apiErrorFromResponse(resp)
	}

	var items []SnapshotInfo
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil && !errors.Is(err, io.EOF) {
		return nil, "", err
	}
	return items, resp.Header.Get("x-next-token"), nil
}

// DeleteSnapshot permanently removes a snapshot (DELETE /templates/:id).
// Deleting the originating sandbox does not cascade-delete its snapshots.
func (c *Client) DeleteSnapshot(ctx context.Context, snapshotID string) error {
	if snapshotID == "" {
		return errors.New("snapshotID is required")
	}
	path := "/templates/" + url.PathEscape(snapshotID)
	return c.doJSON(ctx, http.MethodDelete, path, nil, nil, http.StatusOK, http.StatusNoContent)
}

// Rollback reverts the sandbox to a snapshot (POST /sandboxes/:id/rollback).
// The sandbox process restarts, invalidating pooled data-plane connections, so
// idle connections are dropped and rebuilt lazily on the next call.
func (s *Sandbox) Rollback(ctx context.Context, snapshotID string) (map[string]any, error) {
	if err := s.ensureClient(); err != nil {
		return nil, err
	}
	var result map[string]any
	path := "/sandboxes/" + url.PathEscape(s.SandboxID) + "/rollback"
	if err := s.client.doJSON(ctx, http.MethodPost, path, map[string]any{"snapshotID": snapshotID}, &result, http.StatusOK); err != nil {
		return nil, err
	}
	s.resetConnections()
	return result, nil
}

// ForkResult is the outcome of one requested fork: exactly one of Sandbox or
// Err is set. Sandbox is non-nil when the fork started; Err describes why it
// failed to start.
type ForkResult struct {
	Sandbox *Sandbox
	Err     error
}

// forkRequestTimeout: CubeAPI's FORK_ROUTE_TIMEOUT plus headroom.
const forkRequestTimeout = 1600 * time.Second

// ForkOptions controls Sandbox.Fork: Count (1..100, default 1), per-fork
// TTL Timeout, and client-side RequestTimeout.
type ForkOptions struct {
	Count          *int
	Timeout        *time.Duration
	RequestTimeout *time.Duration
}

// Clone snapshots this sandbox and spins up opts.N copies. The temp snapshot
// is deleted after the last clone is killed. On any create failure the
// successful siblings are killed and the first error is returned.
func (s *Sandbox) Clone(ctx context.Context, opts CloneOptions) ([]*Sandbox, error) {
	if err := s.ensureClient(); err != nil {
		return nil, err
	}
	n := opts.N
	if n <= 0 {
		n = 1
	}
	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = 1
	}

	snapshot, err := s.CreateSnapshot(ctx, "")
	if err != nil {
		return nil, err
	}
	createOne := func() (*Sandbox, error) {
		return s.client.Create(ctx, CreateOptions{TemplateID: snapshot.SnapshotID})
	}

	var (
		mu        sync.Mutex
		clones    []*Sandbox
		firstErr  error
		wg        sync.WaitGroup
		semaphore = make(chan struct{}, min(n, concurrency))
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			clone, err := createOne()
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			clones = append(clones, clone)
		}()
	}
	wg.Wait()

	cleanup := &cloneCleanup{
		client:     s.client,
		snapshotID: snapshot.SnapshotID,
		remaining:  len(clones),
		released:   make(map[string]struct{}, len(clones)),
	}
	for _, clone := range clones {
		clone.cloneCleanup = cleanup
	}

	if firstErr != nil {
		for _, clone := range clones {
			_ = clone.Kill(context.WithoutCancel(ctx))
		}
		// A failed Kill does not release its clone ownership. Force the
		// best-effort snapshot cleanup after every surviving sibling has been
		// handled so a transient teardown failure cannot leak the snapshot.
		cleanup.cleanup(context.WithoutCancel(ctx))
		return nil, firstErr
	}
	return clones, nil
}

// forkResultEntry is one element of the server-side fork response array. The
// server snapshots the source once and derives N sandboxes server-side, so a
// fork is a single HTTP round-trip rather than client orchestration. Exactly one
// of Sandbox or Error is populated per entry.
type forkResultEntry struct {
	Sandbox *Sandbox         `json:"sandbox,omitempty"`
	Error   *forkErrorResult `json:"error,omitempty"`
}

// forkErrorResult is the per-fork error payload ({ code, message }).
type forkErrorResult struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *forkErrorResult) asError() error {
	if e == nil {
		return errors.New("fork failed")
	}
	return &APIError{RetCode: e.Code, Message: e.Message, Kind: apiErrorKindAPI}
}

// Fork derives opts.Count copies of this sandbox server-side. Each fork
// succeeds or fails independently — successes are kept when siblings fail
// (unlike Clone's all-or-nothing). The temporary snapshot is created and
// managed by the backend, so no client-side cleanup is needed. Concurrency is
// governed by the server, not the caller.
func (s *Sandbox) Fork(ctx context.Context, opts ForkOptions) ([]ForkResult, error) {
	count := 1
	if opts.Count != nil {
		count = *opts.Count
	}
	if count < 1 || count > 100 {
		return nil, errors.New("count must be between 1 and 100")
	}
	if err := s.ensureClient(); err != nil {
		return nil, err
	}

	// Fork may legally take minutes; an existing ctx deadline always wins.
	timeout := forkRequestTimeout
	if opts.RequestTimeout != nil {
		timeout = *opts.RequestTimeout
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	// Drop the control client's overall timeout; the deadline lives in ctx.
	// The shared transport is safe for concurrent clients.
	forkHTTP := *s.client.controlHTTP
	forkHTTP.Timeout = 0

	payload := map[string]any{"count": count}
	if opts.Timeout != nil {
		payload["timeout"] = timeoutPayloadSeconds(*opts.Timeout)
	}

	var entries []forkResultEntry
	path := "/sandboxes/" + url.PathEscape(s.SandboxID) + "/fork"
	if err := s.client.doJSONWith(&forkHTTP, ctx, http.MethodPost, path, payload, &entries, http.StatusOK, http.StatusCreated); err != nil {
		return nil, err
	}
	if len(entries) != count {
		return nil, fmt.Errorf("fork returned %d results, expected %d", len(entries), count)
	}

	out := make([]ForkResult, len(entries))
	for i, entry := range entries {
		if entry.Sandbox != nil {
			s.client.attachSandbox(entry.Sandbox)
			out[i] = ForkResult{Sandbox: entry.Sandbox}
		} else {
			out[i] = ForkResult{Err: entry.Error.asError()}
		}
	}
	return out, nil
}

// resetConnections drops pooled data-plane connections so the next request
// reopens a fresh one. Used after rollback restarts the sandbox process.
func (s *Sandbox) resetConnections() {
	if s.client != nil && s.client.dataHTTP != nil {
		s.client.dataHTTP.CloseIdleConnections()
	}
}
