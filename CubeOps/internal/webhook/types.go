// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"encoding/json"
	"time"
)

const (
	EventStreamKey = "cube:v1:shared:sandbox:lifecycle:events"

	EventSandboxCreated = "sandbox.created"
	EventSandboxDeleted = "sandbox.deleted"
	EventSandboxPaused  = "sandbox.paused"
	EventSandboxResumed = "sandbox.resumed"
)

var supportedEvents = map[string]struct{}{
	EventSandboxCreated: {},
	EventSandboxDeleted: {},
	EventSandboxPaused:  {},
	EventSandboxResumed: {},
}

// LifecycleEvent is CubeOps' decoded view of one CubeMaster stream entry.
// StreamID is only a Redis cursor. EventID is the stable identifier exposed to
// webhook receivers and must remain unchanged across retries.
type LifecycleEvent struct {
	StreamID  string
	EventID   string
	Op        string
	SandboxID string
	Timestamp int64
	Payload   json.RawMessage
}

type lifecycleMeta struct {
	TemplateID     string `json:"template_id,omitempty"`
	HostID         string `json:"host_id,omitempty"`
	HostIP         string `json:"host_ip,omitempty"`
	InstanceType   string `json:"instance_type,omitempty"`
	TimeoutSeconds *int   `json:"timeout_seconds,omitempty"`
	AutoPause      bool   `json:"auto_pause,omitempty"`
	AutoResume     bool   `json:"auto_resume,omitempty"`
	CreatedAt      int64  `json:"created_at,omitempty"`
	EndAt          int64  `json:"end_at,omitempty"`
}

type statePayload struct {
	State  string `json:"state"`
	Actor  string `json:"actor,omitempty"`
	Source string `json:"source,omitempty"`
}

// Payload is the stable external webhook contract.
type Payload struct {
	EventID      string         `json:"event_id"`
	Event        string         `json:"event"`
	Timestamp    time.Time      `json:"timestamp"`
	SandboxID    string         `json:"sandbox_id"`
	TemplateID   string         `json:"template_id,omitempty"`
	HostID       string         `json:"host_id,omitempty"`
	HostIP       string         `json:"host_ip,omitempty"`
	InstanceType string         `json:"instance_type,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

func buildPayload(event LifecycleEvent) (Payload, bool) {
	name := ""
	out := Payload{
		EventID:   event.EventID,
		Timestamp: time.UnixMilli(event.Timestamp).UTC(),
		SandboxID: event.SandboxID,
	}
	if out.EventID == "" {
		// Backward-compatible deterministic ID for stream entries written
		// before CubeMaster added event_id. New entries always carry a UUID.
		out.EventID = "legacy:" + event.StreamID
	}

	switch event.Op {
	case "create":
		name = EventSandboxCreated
		var meta lifecycleMeta
		if len(event.Payload) > 0 && json.Unmarshal(event.Payload, &meta) == nil {
			out.TemplateID = meta.TemplateID
			out.HostID = meta.HostID
			out.HostIP = meta.HostIP
			out.InstanceType = meta.InstanceType
			out.Metadata = map[string]any{
				"timeout_seconds": meta.TimeoutSeconds,
				"auto_pause":      meta.AutoPause,
				"auto_resume":     meta.AutoResume,
				"created_at":      meta.CreatedAt,
				"end_at":          meta.EndAt,
			}
		}
	case "delete":
		name = EventSandboxDeleted
		var meta lifecycleMeta
		if len(event.Payload) > 0 && json.Unmarshal(event.Payload, &meta) == nil {
			out.TemplateID = meta.TemplateID
			out.HostID = meta.HostID
			out.HostIP = meta.HostIP
			out.InstanceType = meta.InstanceType
		}
	case "state":
		var state statePayload
		if len(event.Payload) == 0 || json.Unmarshal(event.Payload, &state) != nil {
			return Payload{}, false
		}
		switch state.State {
		case "paused":
			name = EventSandboxPaused
		case "running":
			name = EventSandboxResumed
		default:
			return Payload{}, false
		}
		out.Metadata = map[string]any{
			"actor":  state.Actor,
			"source": state.Source,
		}
	default:
		// Metadata-only update events are internal lifecycle-manager traffic,
		// not an external Webhook event in the current product contract.
		return Payload{}, false
	}
	out.Event = name
	return out, true
}
