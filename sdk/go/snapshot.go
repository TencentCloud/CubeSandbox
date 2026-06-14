// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
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

// CreateSnapshot captures the current sandbox state (POST
// /sandboxes/:id/snapshots). The snapshot outlives the sandbox. An empty name
// lets the server pick one; a known name attaches a new build to it.
func (s *Sandbox) CreateSnapshot(ctx context.Context, name string) (*SnapshotInfo, error) {
	if err := s.ensureClient(); err != nil {
		return nil, err
	}
	var payload map[string]any
	if name != "" {
		payload = map[string]any{"name": name}
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

// Clone snapshots this sandbox and spins up opts.N fresh sandboxes from it,
// then deletes the ephemeral snapshot (best-effort). On any create failure all
// successful siblings are killed and the first error is returned, so a partial
// fan-out never leaks sandboxes.
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
	// Cleanup must run even if ctx is cancelled, matching the Python SDK's
	// unconditional best-effort delete.
	defer func() { _ = s.client.DeleteSnapshot(context.WithoutCancel(ctx), snapshot.SnapshotID) }()

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

	if firstErr != nil {
		for _, clone := range clones {
			_ = clone.Kill(context.WithoutCancel(ctx))
		}
		return nil, firstErr
	}
	return clones, nil
}

// resetConnections drops pooled data-plane connections so the next request
// reopens a fresh one. Used after rollback restarts the sandbox process.
func (s *Sandbox) resetConnections() {
	if s.client != nil && s.client.dataHTTP != nil {
		s.client.dataHTTP.CloseIdleConnections()
	}
}
