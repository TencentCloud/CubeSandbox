// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package webhook delivers sandbox lifecycle events to external HTTP
// endpoints. It is the CLM-side counterpart of CubeAPI's webhook emitter and
// intentionally mirrors its wire protocol (payload shape + HMAC-SHA256
// signature) so a receiver built for one works for the other.
//
// The webhook Manager is an independent consumer of the lifecycle Redis stream
// (cube:v1:shared:sandbox:lifecycle:events) using its OWN consumer group
// (cube-webhook-delivery). It does not touch the registry / proxy / state
// reconciliation — it only reads stream entries, maps them to webhook events
// (create/update/delete/state), delivers them asynchronously, and acks. Every
// event is therefore durable in the stream until delivered+acked, so a crash
// cannot lose it: the group's pending list is reclaimed at startup.
package webhook

import (
	"encoding/json"
	"time"

	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/lifecycle"
)

// EventType identifies a webhook event. The names are the machine-readable
// strings receivers match on (identical to CubeAPI's event names).
type EventType string

const (
	EventCreated EventType = "sandbox.created"
	EventDeleted EventType = "sandbox.deleted"
	EventPaused  EventType = "sandbox.paused"
	EventResumed EventType = "sandbox.resumed"
	// EventUpdated fires when a sandbox's idle timeout is refreshed
	// (set_timeout / TTL refresh). Matches CubeAPI's sandbox.timeout.updated.
	EventUpdated EventType = "sandbox.timeout.updated"
)

// Event is the JSON payload delivered to subscribed endpoints. Extra context
// fields (template_id, host_id, ...) are flattened into the root object,
// matching CubeAPI's webhook payload contract.
type Event struct {
	Event string `json:"event"`
	// EventID is the Redis stream entry ID (e.g. "1725030000000-0"), stable
	// across at-least-once redelivery. Receivers dedupe on it. It is NOT a
	// fresh UUID per delivery — that would defeat idempotency when the CLM
	// crashes before acking and the consumer group redelivers the entry.
	EventID   string `json:"event_id"`
	Timestamp string `json:"timestamp"` // RFC3339 UTC
	SandboxID string `json:"sandbox_id"`

	// Context fields, best-effort (absent for deleted sandboxes if the meta
	// was already evicted from the registry).
	TemplateID     string `json:"template_id,omitempty"`
	HostID         string `json:"host_id,omitempty"`
	HostIP         string `json:"host_ip,omitempty"`
	InstanceType   string `json:"instance_type,omitempty"`
	TimeoutSeconds *int   `json:"timeout_seconds,omitempty"`
	AutoPause      bool   `json:"auto_pause,omitempty"`
	AutoResume     bool   `json:"auto_resume,omitempty"`
	CreatedAt      int64  `json:"created_at,omitempty"`
	EndAt          int64  `json:"end_at,omitempty"`

	// State transitions only.
	State  string `json:"state,omitempty"` // "paused" | "running"
	Actor  string `json:"actor,omitempty"`
	Source string `json:"source,omitempty"`

	// Timeout (seconds) is populated only on sandbox.timeout.updated, matching
	// CubeAPI's payload for that event. Omitted (via omitempty) elsewhere.
	Timeout *int `json:"timeout,omitempty"`
}

// NewCreated builds a sandbox.created event from the full meta carried by the
// create stream entry.
func NewCreated(meta lifecycle.SandboxLifecycleMeta, eventID string, tsMs int64) Event {
	e := fromMeta(meta.SandboxID, &meta, eventID, tsMs)
	e.Event = string(EventCreated)
	return e
}

// NewUpdated builds a sandbox.timeout.updated event from the refreshed meta
// carried by the update stream entry.
func NewUpdated(meta lifecycle.SandboxLifecycleMeta, eventID string, tsMs int64) Event {
	e := fromMeta(meta.SandboxID, &meta, eventID, tsMs)
	e.Event = string(EventUpdated)
	e.Timeout = meta.TimeoutSeconds
	return e
}

// NewDeleted builds a sandbox.deleted event. The delete stream entry carries
// no payload, so meta is whatever the caller captured from the registry before
// eviction; it may be nil (template_id etc. then omitted).
func NewDeleted(sandboxID string, meta *lifecycle.SandboxLifecycleMeta, eventID string, tsMs int64) Event {
	e := fromMeta(sandboxID, meta, eventID, tsMs)
	e.Event = string(EventDeleted)
	return e
}

// NewState builds a sandbox.paused / sandbox.resumed event from a state stream
// entry. It returns ok=false (and a zero Event) when the payload is nil or the
// state is not a terminal state worth broadcasting.
func NewState(sandboxID string, sp *lifecycle.StatePayload, meta *lifecycle.SandboxLifecycleMeta, eventID string, tsMs int64) (Event, bool) {
	var typ EventType
	switch {
	case sp == nil:
		return Event{}, false
	case sp.State == lifecycle.StatePaused:
		typ = EventPaused
	case sp.State == lifecycle.StateRunning:
		typ = EventResumed
	default:
		return Event{}, false
	}
	e := fromMeta(sandboxID, meta, eventID, tsMs)
	e.Event = string(typ)
	e.State = sp.State
	e.Actor = sp.Actor
	e.Source = sp.Source
	return e, true
}

// fromMeta seeds an Event with sandbox identity + context fields from the
// registry meta (if available). tsMs is the stream entry timestamp in unix ms;
// zero falls back to now.
func fromMeta(sandboxID string, meta *lifecycle.SandboxLifecycleMeta, eventID string, tsMs int64) Event {
	if tsMs <= 0 {
		tsMs = time.Now().UnixMilli()
	}
	e := Event{
		EventID:   eventID,
		Timestamp: time.UnixMilli(tsMs).UTC().Format(time.RFC3339),
		SandboxID: sandboxID,
	}
	if meta != nil {
		e.TemplateID = meta.TemplateID
		e.HostID = meta.HostID
		e.HostIP = meta.HostIP
		e.InstanceType = meta.InstanceType
		e.TimeoutSeconds = meta.TimeoutSeconds
		e.AutoPause = meta.AutoPause
		e.AutoResume = meta.AutoResume
		e.CreatedAt = meta.CreatedAt
		e.EndAt = meta.EndAt
	}
	return e
}

// MarshalBody returns the exact bytes that get signed and POSTed. The same
// slice must be used for both the HMAC and the request body so a receiver can
// recompute the signature over the body it reads.
func (e Event) MarshalBody() ([]byte, error) {
	return json.Marshal(e)
}
