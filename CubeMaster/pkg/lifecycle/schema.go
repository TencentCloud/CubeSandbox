// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package lifecycle owns the cross-process metadata channel used by
// CubeProxy-sidecar to track auto-pause / auto-resume decisions.
//
// CubeMaster is the single writer for the canonical view:
//
//   - cube:v1:shared:sandbox:lifecycle:meta    HSet, field=sandboxID,
//     value=JSON snapshot. Sidecars HGETALL it on startup to bootstrap
//     the registry.
//   - cube:v1:shared:sandbox:lifecycle:events  Stream, append-only event
//     log of create/delete operations. Sidecars consume via XREADGROUP for
//     incremental updates after the bootstrap.
//
// Updates (pause/resume action) intentionally do NOT publish to the stream:
// state transitions are driven and observed by the sidecar itself, so making
// CubeMaster also publish them would just be a redundant round trip.
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
