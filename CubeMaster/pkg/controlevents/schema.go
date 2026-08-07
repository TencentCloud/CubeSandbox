// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package controlevents owns the Cubemaster↔Cubemaster control-plane event
// channel backed by a Redis Stream.
//
// MySQL remains the source of truth for node metadata. The stream only
// accelerates multi-replica in-memory convergence after a successful DB write.
// Each Cubemaster replica independently XREADs the stream (broadcast); a shared
// consumer group is intentionally NOT used.
//
// Stream: cube:v1:shared:master:control:events
package controlevents

import "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/rediskey"

// EventStreamKey is the append-only control-plane event stream.
var EventStreamKey = rediskey.MasterControlEvents()

const (
	// EventStreamMaxLen caps stream growth. Replicas bootstrap cordon state
	// from MySQL (nodemeta reload), so trimmed events are recovered on the
	// next full sync.
	EventStreamMaxLen = 10000
)

// Event op codes carried in stream entries.
const (
	OpNodeIsolate   = "node.isolate"
	OpNodeUnisolate = "node.unisolate"
)

// Stream entry field names.
const (
	FieldOp        = "op"
	FieldNodeID    = "node_id"
	FieldPayload   = "payload"
	FieldTimestamp = "ts"
)

// IsolationPayload is the JSON body for node.isolate / node.unisolate events.
type IsolationPayload struct {
	SchedulingDisabled bool   `json:"scheduling_disabled"`
	UpdatedAtUnixMs    int64  `json:"updated_at_unix_ms,omitempty"`
	Origin             string `json:"origin,omitempty"`
}

// Event is a decoded control-plane stream entry.
type Event struct {
	StreamID  string
	Op        string
	NodeID    string
	Payload   []byte
	Timestamp int64
}
