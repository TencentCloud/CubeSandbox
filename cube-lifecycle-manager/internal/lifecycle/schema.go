// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package lifecycle is the sidecar-local mirror of
// CubeMaster/pkg/lifecycle. The two MUST stay byte-compatible: CubeMaster is
// the single writer, the sidecar is a pure consumer.
//
// We do not import the CubeMaster module directly because it would drag in
// MySQL, gRPC, scheduler, and a host of other heavy dependencies that have no
// place in the sidecar. The schema is small enough that copying it (with a
// pointer to the canonical definition) is cheaper than the cross-module wire.
//
// Source of truth:
//
//	CubeMaster/pkg/lifecycle/schema.go
//
// Whenever you change one side, change the other in the same commit.
package lifecycle

const (
	// MetaKey is the HSet snapshot of every live sandbox.
	MetaKey = "cube:v1:shared:sandbox:lifecycle:meta"

	// EventStreamKey is the append-only stream of create/delete events.
	EventStreamKey = "cube:v1:shared:sandbox:lifecycle:events"

	// EventStreamMaxLen caps the stream so an offline sidecar cannot drive
	// unbounded Redis growth.
	EventStreamMaxLen = 100000
)

// StateKey returns the per-sandbox pause/resume coordination key. Values are
// "running" | "pausing" | "paused" | "resuming". The sidecar uses SETNX with
// TTL to coordinate concurrent pause/resume across replicas.
func StateKey(sandboxID string) string {
	return "cube:v1:shared:sandbox:lifecycle:state:" + sandboxID
}

// Op codes carried in stream entries.
const (
	OpCreate = "create"
	OpDelete = "delete"
	OpUpdate = "update"
	// OpState carries a runtime state transition (paused / running) emitted
	// by CubeMaster after a successful pause / resume RPC. The payload is a
	// StatePayload. See CubeMaster/pkg/lifecycle/schema.go for the canonical
	// definition; the CLM consumes these events in statesync.Handle.
	OpState = "state"
)

// Stream entry field names.
const (
	FieldOp        = "op"
	FieldSandboxID = "sandbox_id"
	FieldPayload   = "payload"
	FieldTimestamp = "ts"
)

// SandboxLifecycleMeta mirrors CubeMaster/pkg/lifecycle.SandboxLifecycleMeta.
type SandboxLifecycleMeta struct {
	SandboxID    string `json:"sandbox_id"`
	TemplateID   string `json:"template_id,omitempty"`
	HostID       string `json:"host_id,omitempty"`
	HostIP       string `json:"host_ip,omitempty"`
	InstanceType string `json:"instance_type,omitempty"`
	// *int so nil (legacy absent) ≠ explicit 0. See docs/guide/lifecycle.md.
	TimeoutSeconds *int  `json:"timeout_seconds,omitempty"`
	AutoPause      bool  `json:"auto_pause,omitempty"`
	AutoResume     bool  `json:"auto_resume,omitempty"`
	CreatedAt      int64 `json:"created_at,omitempty"`
	EndAt          int64 `json:"end_at,omitempty"`
}

// TimeoutSecondsPtr is a convenience constructor for the TimeoutSeconds
// pointer field. Prefer it over inline &v so intent reads clearly.
func TimeoutSecondsPtr(v int) *int {
	return &v
}

// State values carried by StatePayload.State. Only terminal states are
// broadcast on the stream — transition markers ("pausing", "resuming")
// stay private to the CLM's state-key coordination logic.
const (
	StatePaused  = "paused"
	StateRunning = "running"
)

// Actor values distinguishing who initiated the state change.
const (
	ActorCubeMaster = "cubemaster"
	ActorCLM        = "clm"
)

// StatePayload mirrors CubeMaster/pkg/lifecycle.StatePayload. Whenever you
// change one side, change the other in the same commit.
type StatePayload struct {
	State  string `json:"state"`
	Actor  string `json:"actor,omitempty"`
	Source string `json:"source,omitempty"`
}
