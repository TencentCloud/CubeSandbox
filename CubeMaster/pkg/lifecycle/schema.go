// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package lifecycle owns the cross-process metadata channel used by
// CubeProxy-sidecar to track auto-pause / auto-resume decisions.
//
// CubeMaster is the single writer for the canonical view:
//
//   - cube:sandbox:meta            HSet, field=sandboxID, value=JSON snapshot.
//     Sidecars HGETALL it on startup to bootstrap the registry.
//   - cube:sandbox:events          Stream, append-only event log of
//     create/delete operations. Sidecars consume via XREADGROUP for
//     incremental updates after the bootstrap.
//
// Updates (pause/resume action) intentionally do NOT publish to the stream:
// state transitions are driven and observed by the sidecar itself, so making
// CubeMaster also publish them would just be a redundant round trip.
package lifecycle

// Redis key / field constants. Keep them centralized so the sidecar (Go) and
// any other consumer can import the same source of truth.
const (
	// MetaKey is the HSet snapshot of every live sandbox the sidecar should
	// know about. Field = sandbox ID, value = JSON-encoded SandboxLifecycleMeta.
	MetaKey = "cube:sandbox:meta"

	// EventStreamKey is the append-only stream of create/delete events. The
	// sidecar maintains a consumer group on it; entries trim with MAXLEN ~.
	EventStreamKey = "cube:sandbox:events"

	// EventStreamMaxLen caps the stream so an offline sidecar cannot drive
	// unbounded Redis growth. Sidecars also bootstrap from MetaKey, so any
	// trimmed events are recovered on the next full sync.
	EventStreamMaxLen = 100000
)

// Event op codes carried in stream entries.
const (
	OpCreate = "create"
	OpDelete = "delete"
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
	SandboxID      string `json:"sandbox_id"`
	TemplateID     string `json:"template_id,omitempty"`
	HostID         string `json:"host_id,omitempty"`
	HostIP         string `json:"host_ip,omitempty"`
	InstanceType   string `json:"instance_type,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	AutoPause      bool   `json:"auto_pause,omitempty"`
	AutoResume     bool   `json:"auto_resume,omitempty"`
	// CreatedAt is unix milliseconds. Sidecars use it as the initial
	// "last active" baseline before they ever observe a real request.
	CreatedAt int64 `json:"created_at,omitempty"`
}
