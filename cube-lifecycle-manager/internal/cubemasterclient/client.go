// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package cubemasterclient is the sidecar's tiny HTTP client for CubeMaster.
// It calls the same /cube/sandbox/update endpoint that CubeAPI uses; we go
// directly here to avoid the sidecar → CubeAPI → CubeMaster round-trip.
package cubemasterclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// CubeMaster ret_code constants the sidecar reasons about. The full set
// lives in CubeMaster/api/services/errorcode/v1/errorcode.proto; we mirror
// only the codes we need to react to here, keeping the sidecar free of a
// build-time dependency on the master proto.
const (
	// RetCodeSuccess is CubeMaster's "operation succeeded" code.
	RetCodeSuccess = 200

	// RetCodeInvalidParamFormat is reused by CubeMaster's pause/resume path
	// for "sandbox does not exist" — the meta lookup misses, surfaced as
	// ret_msg "key not found". Treat as a hard NotFound for the caller.
	RetCodeInvalidParamFormat = 130483

	// RetCodeTaskStateInvalid is returned when the requested transition is
	// a no-op (e.g. pause on an already-paused sandbox, or resume on a
	// running one). Idempotent from the caller's POV.
	RetCodeTaskStateInvalid = 130490
)

const (
	// KillReasonRequest is an explicit Sandbox.kill() / DELETE call from a
	// human or SDK client. Used by CubeAPI's kill_sandbox path.
	KillReasonRequest = "request"

	// KillReasonTimeout is the sidecar sweeper reaping an idle sandbox that
	// did not opt into auto_pause (lifecycle.on_timeout=kill, the default).
	KillReasonTimeout = "timeout"

	// KillReasonOrphaned is reserved for future use: a sandbox observed on a
	// node but missing from the registry / Redis source-of-truth. Mirrors
	// e2b's orphan reaper.
	KillReasonOrphaned = "orphaned"
)

// APIError is returned by the client whenever CubeMaster replies with a
// non-success ret_code. Callers can errors.As-extract it to react to
// specific conditions (e.g. "sandbox already paused" → treat as success).
type APIError struct {
	RetCode int
	RetMsg  string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("cubemaster returned ret_code=%d msg=%q", e.RetCode, e.RetMsg)
}

// IsNotFound reports whether the master replied with the "sandbox does not
// exist" ret_code. Sidecar uses this to evict stale registry entries instead
// of retrying a doomed pause/resume forever.
func (e *APIError) IsNotFound() bool {
	return e != nil && e.RetCode == RetCodeInvalidParamFormat
}

// IsAlreadyInState reports whether the master refused the transition because
// the sandbox is already in the desired state. From the sidecar's POV this
// is success: the sandbox is already where we wanted it, no retry needed.
func (e *APIError) IsAlreadyInState() bool {
	return e != nil && e.RetCode == RetCodeTaskStateInvalid
}

// Client is a thin wrapper around http.Client + base URL. Concurrency-safe.
type Client struct {
	baseURL string
	httpc   *http.Client
}

func New(baseURL string, timeout time.Duration) *Client {
	return &Client{
		baseURL: baseURL,
		httpc:   &http.Client{Timeout: timeout},
	}
}

// updateRequest mirrors CubeMaster pkg/service/sandbox/types.UpdateRequest.
type updateRequest struct {
	RequestID    string `json:"requestID"`
	SandboxID    string `json:"sandbox_id"`
	InstanceType string `json:"instance_type"`
	Action       string `json:"action"` // "pause" | "resume"
}

type updateResponse struct {
	Ret struct {
		RetCode int    `json:"ret_code"`
		RetMsg  string `json:"ret_msg"`
	} `json:"ret"`
}

type killRequest struct {
	RequestID    string `json:"requestID"`
	SandboxID    string `json:"sandbox_id"`
	InstanceType string `json:"instance_type"`
	Sync         bool   `json:"sync"`
	KillReason   string `json:"kill_reason,omitempty"`
}

// Pause asks CubeMaster to pause the given sandbox. instanceType is required
// by the master; for the cubebox runtime that's "cubebox".
//
// Returns nil on success or when the sandbox is already paused. Returns an
// *APIError for any non-success ret_code; use APIError.IsNotFound /
// IsAlreadyInState to classify.
func (c *Client) Pause(ctx context.Context, sandboxID, instanceType string) error {
	return c.update(ctx, sandboxID, instanceType, "pause")
}

// Resume asks CubeMaster to resume the given sandbox. Same error semantics
// as Pause.
func (c *Client) Resume(ctx context.Context, sandboxID, instanceType string) error {
	return c.update(ctx, sandboxID, instanceType, "resume")
}

// Kill asks CubeMaster to destroy the given sandbox.
func (c *Client) Kill(ctx context.Context, sandboxID, instanceType, reason string) error {
	if sandboxID == "" || instanceType == "" {
		return errors.New("sandbox_id and instance_type are required")
	}

	body, err := json.Marshal(killRequest{
		RequestID:    uuid.NewString(),
		SandboxID:    sandboxID,
		InstanceType: instanceType,
		Sync:         true,
		KillReason:   reason,
	})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		c.baseURL+"/cube/sandbox", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("http %d: %s", resp.StatusCode, raw)
	}

	var ur updateResponse
	if err := json.Unmarshal(raw, &ur); err != nil {
		return fmt.Errorf("decode response: %w (body=%q)", err, raw)
	}
	if ur.Ret.RetCode == RetCodeSuccess {
		return nil
	}
	return &APIError{RetCode: ur.Ret.RetCode, RetMsg: ur.Ret.RetMsg}
}

func (c *Client) update(ctx context.Context, sandboxID, instanceType, action string) error {
	if sandboxID == "" || instanceType == "" {
		return errors.New("sandbox_id and instance_type are required")
	}

	body, err := json.Marshal(updateRequest{
		RequestID:    uuid.NewString(),
		SandboxID:    sandboxID,
		InstanceType: instanceType,
		Action:       action,
	})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/cube/sandbox/update", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("http %d: %s", resp.StatusCode, raw)
	}

	var ur updateResponse
	if err := json.Unmarshal(raw, &ur); err != nil {
		return fmt.Errorf("decode response: %w (body=%q)", err, raw)
	}
	if ur.Ret.RetCode == RetCodeSuccess {
		return nil
	}
	return &APIError{RetCode: ur.Ret.RetCode, RetMsg: ur.Ret.RetMsg}
}
