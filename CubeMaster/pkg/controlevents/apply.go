// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package controlevents

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
)

// ApplyFunc updates the local process view for a node cordon change without
// writing MySQL. Provided by the wiring layer (typically nodemeta) to avoid an
// import cycle between controlevents and nodemeta.
type ApplyFunc func(ctx context.Context, nodeID string, disabled bool) error

// NewIsolationHandler returns a Handler that dispatches isolate/unisolate ops.
func NewIsolationHandler(apply ApplyFunc) Handler {
	return func(ctx context.Context, ev Event) error {
		return applyIsolationEvent(ctx, ev, apply)
	}
}

func applyIsolationEvent(ctx context.Context, ev Event, apply ApplyFunc) error {
	if apply == nil {
		return nil
	}
	if ev.NodeID == "" {
		return fmt.Errorf("missing node_id")
	}

	var disabled bool
	switch ev.Op {
	case OpNodeIsolate:
		disabled = true
	case OpNodeUnisolate:
		disabled = false
	default:
		log.G(ctx).Debugf("controlevents: ignoring unknown op=%s", ev.Op)
		return nil
	}

	if len(ev.Payload) > 0 {
		var p IsolationPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			log.G(ctx).Warnf("controlevents: bad payload op=%s node=%s: %v", ev.Op, ev.NodeID, err)
			// Fall through with op-derived disabled; payload is advisory.
		} else if p.SchedulingDisabled != disabled {
			// Prefer explicit payload when present and consistent with intent;
			// if inconsistent, trust the op (stream field is authoritative).
			log.G(ctx).Warnf("controlevents: payload scheduling_disabled=%v disagrees with op=%s node=%s; using op",
				p.SchedulingDisabled, ev.Op, ev.NodeID)
		}
	}

	return apply(ctx, ev.NodeID, disabled)
}
