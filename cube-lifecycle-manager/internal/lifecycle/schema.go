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

import "strings"

const (
	// MetaKey is the HSet snapshot of every live sandbox.
	MetaKey = "cube:v1:shared:sandbox:lifecycle:meta"

	// EventStreamKey is the append-only stream of create/delete events.
	EventStreamKey = "cube:v1:shared:sandbox:lifecycle:events"

	// EventStreamMaxLen caps the stream so an offline sidecar cannot drive
	// unbounded Redis growth.
	EventStreamMaxLen = 100000

	// EventChannel carries best-effort state-change wakeup hints. Redis
	// state keys remain the source of truth.
	EventChannel = "cube:v1:shared:sandbox:lifecycle:notify"
)

// StateKilled is a state-key marker for a sandbox killed by the sweeper. It is
// intentionally not part of the {paused, running} terminal set emitted on the
// events stream.
const StateKilled = "killed"

// In-flight transition markers held in the state key while a pause / resume /
// kill is being driven. They are private to the CLM's state-key coordination;
// only terminal states ever appear on the events stream.
const (
	StatePausing  = "pausing"
	StateResuming = "resuming"
	StateKilling  = "killing"
)

// TransitionValue returns the value written into the state key while a
// transition is in flight: "<transition>@<owner>". The owner suffix makes
// cross-replica ownership explicit so that only the replica which took the
// lock can commit or release it (active-standby, issue #1211). Legacy writers
// stored the bare transition word, so readers must match with IsTransition,
// never with a plain ==. An empty owner yields the bare form, which degrades
// to the legacy pre-owner semantics.
func TransitionValue(transition, owner string) string {
	if owner == "" {
		return transition
	}
	return transition + "@" + owner
}

// IsTransition reports whether cur denotes the given in-flight transition,
// accepting both the bare legacy form ("pausing") and the owner-tagged form
// ("pausing@<owner>") so a rolling upgrade never misreads a peer's lock.
func IsTransition(cur, transition string) bool {
	return cur == transition || strings.HasPrefix(cur, transition+"@")
}

// StateNotify is a best-effort wakeup hint. Consumers must read the state key
// after receiving it instead of trusting the payload as current truth.
type StateNotify struct {
	SandboxID string `json:"sandbox_id"`
}

// StateKey returns the per-sandbox coordination key. Terminal values are
// running, paused, and killed; in-flight transitions are stored as
// owner-tagged markers ("pausing@<owner>", "resuming@<owner>",
// "killing@<owner>") — see TransitionValue / IsTransition.
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
