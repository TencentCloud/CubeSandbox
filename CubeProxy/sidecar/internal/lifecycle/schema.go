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
//   /data/cube-opensource-dev/CubeSandbox/CubeMaster/pkg/lifecycle/schema.go
//
// Whenever you change one side, change the other in the same commit.
package lifecycle

const (
	// MetaKey is the HSet snapshot of every live sandbox.
	MetaKey = "cube:sandbox:meta"

	// EventStreamKey is the append-only stream of create/delete events.
	EventStreamKey = "cube:sandbox:events"

	// EventStreamMaxLen caps the stream so an offline sidecar cannot drive
	// unbounded Redis growth.
	EventStreamMaxLen = 100000

	// StateKeyPrefix + sandboxID stores "running" | "pausing" | "paused" |
	// "resuming". The sidecar uses these as cross-process locks (SETNX with
	// TTL) to coordinate concurrent pause/resume.
	StateKeyPrefix = "cube:sandbox:state:"
)

// Op codes carried in stream entries.
const (
	OpCreate = "create"
	OpDelete = "delete"
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
	SandboxID      string `json:"sandbox_id"`
	TemplateID     string `json:"template_id,omitempty"`
	HostID         string `json:"host_id,omitempty"`
	HostIP         string `json:"host_ip,omitempty"`
	InstanceType   string `json:"instance_type,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	AutoPause      bool   `json:"auto_pause,omitempty"`
	AutoResume     bool   `json:"auto_resume,omitempty"`
	CreatedAt      int64  `json:"created_at,omitempty"`
}
