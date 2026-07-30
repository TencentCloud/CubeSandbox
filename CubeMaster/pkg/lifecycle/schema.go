// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package lifecycle owns the cross-process metadata channel used by
// CLM to track auto-pause / auto-resume decisions.
//
// CubeMaster is the single writer for the canonical view:
//
//   - cube:v1:shared:sandbox:lifecycle:meta    HSet, field=sandboxID,
//     value=JSON snapshot. CLM HGETALL it on startup to bootstrap
//     the registry.
//   - cube:v1:shared:sandbox:lifecycle:events  Stream, append-only event
//     log of create/delete/update/state operations. CLM consumes via
//     XREADGROUP for incremental updates after the bootstrap.
//
// Stream events cover two classes of change:
//
//  1. Metadata mutations: create / delete / update. These carry a full
//     SandboxLifecycleMeta payload (except delete). The CLM mirrors
//     them into its in-memory registry and pushes the meta to CubeProxy.
//
//  2. Runtime state mutations: state. Emitted after a successful
//     pause / resume RPC (see sandbox_update.go). Payload is a
//     StatePayload describing the new terminal state ("paused" / "running").
//     The CLM reconciles Redis state key + CubeProxy state dict so
//     externally driven pause/resume calls do not desync the fleet.
//
// State keys (cube:v1:shared:sandbox:lifecycle:state:<id>) remain
// exclusively written by the CLM — CubeMaster only signals intent
// via the stream. This preserves the SETNX-based transition-lock semantics
// (pausing / resuming) owned by the CLM's sweeper and resumer.
package lifecycle

import "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/rediskey"

// Redis key constants. Keep them centralized so the sidecar (Go) and any
// other consumer can import the same source of truth.
var (
	// MetaKey is the HSet snapshot of every live sandbox the sidecar should
	// know about. Field = sandbox ID, value = JSON-encoded SandboxLifecycleMeta.
	MetaKey = rediskey.SandboxLifecycleMeta()

	// EventStreamKey is the append-only stream of create/delete events. The
	// sidecar maintains a consumer group on it; entries trim with MAXLEN ~.
	EventStreamKey = rediskey.SandboxLifecycleEvents()
)

const (
	// EventStreamMaxLen caps the stream so an offline sidecar cannot drive
	// unbounded Redis growth. Sidecars also bootstrap from MetaKey, so any
	// trimmed events are recovered on the next full sync.
	EventStreamMaxLen = 100000
)

// Event op codes carried in stream entries.
const (
	OpCreate = "create"
	OpDelete = "delete"
	OpUpdate = "update"
	// OpState carries a runtime state transition (paused / running) after
	// a successful pause / resume RPC. The payload is a StatePayload. It
	// does NOT mutate MetaKey: state is a runtime attribute the CLM
	// tracks separately (state keys), and MetaKey should stay stable
	// across pause/resume cycles.
	OpState = "state"
)

// Stream entry field names. Stream values are flat key/value pairs in redigo,
// so we model the schema as constants rather than a struct.
const (
	FieldOp        = "op"
	FieldSandboxID = "sandbox_id"
	FieldPayload   = "payload"
	FieldTimestamp = "ts"
)

// SandboxLifecycleMeta is the JSON value stored under MetaKey[sandboxID] and
// also the payload field of OpCreate stream entries. OpDelete entries omit
// the payload field — the sandbox ID is enough to drop a registry entry.
type SandboxLifecycleMeta struct {
	SandboxID    string `json:"sandbox_id"`
	TemplateID   string `json:"template_id,omitempty"`
	HostID       string `json:"host_id,omitempty"`
	HostIP       string `json:"host_ip,omitempty"`
	InstanceType string `json:"instance_type,omitempty"`
	// *int so nil (legacy absent) ≠ explicit 0. See docs/guide/lifecycle.md.
	TimeoutSeconds *int `json:"timeout_seconds,omitempty"`
	AutoPause      bool `json:"auto_pause,omitempty"`
	AutoResume     bool `json:"auto_resume,omitempty"`
	// CreatedAt is unix milliseconds. Sidecars use it as the initial
	// "last active" baseline before they ever observe a real request.
	CreatedAt int64 `json:"created_at,omitempty"`
	// EndAt is unix milliseconds, the projected next-timeout instant
	// (CreatedAt + TimeoutSeconds*1000). Filled in by the master so
	// API consumers (and the SDK's get_info endpoint) can return it
	// without recomputing.
	EndAt int64 `json:"end_at,omitempty"`
}

// State values carried by StatePayload.State. Only terminal states are
// broadcast — transition markers ("pausing", "resuming") remain private to
// the CLM's state-key coordination logic.
const (
	StatePaused  = "paused"
	StateRunning = "running"
)

// Actor values distinguishing who initiated the state change. The CLM
// uses this to decide whether to reconcile registry / proxy dict; events
// authored by the CLM itself (actor="clm") are typically no-ops on the
// consumer side because the CLM has already made the updates locally.
const (
	ActorCubeMaster = "cubemaster"
	ActorCLM        = "clm"
)

// StatePayload is the JSON body of an OpState stream entry.
//
// Emitted by CubeMaster after a successful pause / resume RPC so the
// CLM can synchronise Redis state, in-memory registry, and CubeProxy's
// per-worker state dict with externally-driven state changes (e.g. the SDK
// calling connect() to resume a sandbox the sweeper had paused).
type StatePayload struct {
	// State is the new terminal state. Must be one of StatePaused / StateRunning.
	State string `json:"state"`
	// Actor identifies the driver of the state change. Used for logging
	// and to give the CLM a hook for cheap no-op detection.
	Actor string `json:"actor,omitempty"`
	// Source is a free-form label for the trigger (e.g. "api", "admin").
	// Purely informational; not consumed by the CLM's decision logic.
	Source string `json:"source,omitempty"`
}
