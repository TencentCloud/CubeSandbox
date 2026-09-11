// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package cubemasterclient is CLM's tiny HTTP client for CubeMaster.
// It calls the same /cube/sandbox/update endpoint that CubeAPI uses; we go
// directly here to avoid the CLM → CubeAPI → CubeMaster round-trip.
package cubemasterclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// CubeMaster ret_code constants CLM reasons about. The full set
// lives in pkgs/proto/services/errorcode/v1/errorcode.proto; we mirror
// only the codes we need to react to here, keeping CLM free of a
// build-time dependency on the master proto.
const (
	// RetCodeSuccess is CubeMaster's "operation succeeded" code.
	RetCodeSuccess = 200

	// RetCodeMasterParamsError is CubeMaster's generic params error.
	// Pause uses it both for real Begin() failures and for "already has
	// pause snapshot" (see pausesnap.Begin). Only the latter is a no-op.
	RetCodeMasterParamsError = 130400

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

	// KillReasonTimeout is the CLM sweeper reaping an idle sandbox that
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
	RetCode         int
	RetMsg          string
	ResumeCompleted bool
}

func (e *APIError) Error() string {
	return fmt.Sprintf("cubemaster returned ret_code=%d msg=%q", e.RetCode, e.RetMsg)
}

// IsNotFound reports whether the master replied with the "sandbox does not
// exist" ret_code. CLM uses this to evict stale registry entries instead
// of retrying a doomed pause/resume forever.
func (e *APIError) IsNotFound() bool {
	return e != nil && e.RetCode == RetCodeInvalidParamFormat
}

// IsPauseSuperseded means Master rejected a queued auto-pause after a resume
// changed the shared state. It is not a successful pause.
func (e *APIError) IsPauseSuperseded() bool {
	return e != nil && e.RetCode == retCodeConflict && e.RetMsg == pauseSupersededMessage
}

// Wire contract mirrored from CubeMaster's ErrorCode_Conflict and
// pkg/service/sandbox/sandbox_lifecycle_state.go. Keep both sides and their
// literal contract tests in sync: the code also represents lock contention,
// so the message is part of the protocol, not freely editable display text.
const (
	retCodeConflict        = 130409
	pauseSupersededMessage = "auto-pause superseded by lifecycle state change"
)

// Info status values mirror ContainerState in
// pkgs/proto/services/cubebox/v1/cubebox.proto without importing the proto module.
const (
	statusRunning = 1 // CONTAINER_RUNNING
	statusPaused  = 5 // CONTAINER_PAUSED
)

// alreadyHasPauseSnapshotMarker is the pausesnap.Begin message CubeMaster
// wraps as 130400. Other 130400 Begin failures must not be treated as success.
const alreadyHasPauseSnapshotMarker = "already has pause snapshot"

// IsAlreadyInState reports whether the master refused the transition because
// the sandbox is already in the desired state. From CLM's POV this
// is success: the sandbox is already where we wanted it, no retry needed.
func (e *APIError) IsAlreadyInState() bool {
	if e == nil {
		return false
	}
	if e.RetCode == RetCodeTaskStateInvalid {
		return true
	}
	return e.RetCode == RetCodeMasterParamsError &&
		strings.Contains(e.RetMsg, alreadyHasPauseSnapshotMarker)
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
	RequestID              string `json:"requestID"`
	SandboxID              string `json:"sandbox_id"`
	InstanceType           string `json:"instance_type"`
	Action                 string `json:"action"` // "pause" | "resume"
	ExpectedLifecycleState string `json:"expected_lifecycle_state,omitempty"`
}

type updateResponse struct {
	ResumeCompleted bool `json:"resume_completed"`
	Ret             struct {
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

// Pause asks CubeMaster to auto-pause the given sandbox. The caller must first
// acquire CLM's pausing marker; Master revalidates it under the lifecycle lock.
// instanceType is required; for the cubebox runtime that's "cubebox".
//
// Returns nil on success, or an *APIError for any non-success ret_code; use
// IsNotFound, IsAlreadyInState, or IsPauseSuperseded to classify it.
func (c *Client) Pause(ctx context.Context, sandboxID, instanceType string) error {
	return c.update(ctx, sandboxID, instanceType, "pause")
}

// Resume asks CubeMaster to resume the given sandbox. Same error semantics
// as Pause.
func (c *Client) Resume(ctx context.Context, sandboxID, instanceType string) error {
	return c.update(ctx, sandboxID, instanceType, "resume")
}

// SandboxState provides an authoritative fallback when a reconciliation task
// outlives the short-lived Redis state marker.
func (c *Client) SandboxState(ctx context.Context, sandboxID, instanceType string) (string, error) {
	query := url.Values{"sandbox_id": {sandboxID}, "instance_type": {instanceType}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/cube/sandbox/info?"+query.Encode(), nil)
	if err != nil {
		return "", err
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("sandbox state: http %d", resp.StatusCode)
	}
	var body struct {
		updateResponse
		Data []struct {
			SandboxID string `json:"sandbox_id"`
			Status    int    `json:"status"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", err
	}
	if body.Ret.RetCode != RetCodeSuccess {
		return "", &APIError{RetCode: body.Ret.RetCode, RetMsg: body.Ret.RetMsg}
	}
	for _, item := range body.Data {
		if item.SandboxID != sandboxID {
			continue
		}
		switch item.Status {
		case statusRunning:
			return "running", nil
		case statusPaused:
			return "paused", nil
		}
	}
	return "", errors.New("sandbox has no confirmed running/paused state")
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

	var expectedState string
	if action == "pause" {
		expectedState = "pausing"
	}
	body, err := json.Marshal(updateRequest{
		RequestID:              uuid.NewString(),
		SandboxID:              sandboxID,
		InstanceType:           instanceType,
		Action:                 action,
		ExpectedLifecycleState: expectedState,
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
	return &APIError{RetCode: ur.Ret.RetCode, RetMsg: ur.Ret.RetMsg, ResumeCompleted: action == "resume" && ur.ResumeCompleted}
}
