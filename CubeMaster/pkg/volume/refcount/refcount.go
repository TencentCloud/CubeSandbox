// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package refcount maintains the cross-node reference count stored in
// t_cube_volume.refcount.
//
// Cubelet tracks a node-local ref-count per plugin_volume (how many sandboxes
// on that node use it). Whenever the node-local reference state flips it reports
// an event to CubeMaster on the sandbox create/destroy response ext_info:
// referenced=1 when the node started referencing the volume (0→1) and
// referenced=0 when it stopped (1→0); repeat references on the same node emit
// no event. CubeMaster turns each event into a +1 / -1 adjustment here so it
// always knows how many nodes reference each volume and can refuse deletion
// while any do.
package refcount

import (
	"context"
	"encoding/json"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/dao"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"gorm.io/gorm"
)

// ExtInfoKey is the create/destroy response ext_info key under which Cubelet
// reports node-level volume ref-count transitions. It MUST match the Cubelet
// constant constants.CubeExtVolumeRefEvents.
const ExtInfoKey = "cube-volume-refcount-events"

// Event is a single node-level reference-state change for a volume.
// Referenced is 1 when the node started referencing the volume (0→1) and 0
// when it stopped (1→0). Repeat references on the same node produce no event.
type Event struct {
	VolumeID   string `json:"volume_id"`
	Referenced int    `json:"referenced"`
}

// ApplyFromExtInfo parses the volume reference-state events embedded in a
// Cubelet create/destroy response ext_info map and applies each one to
// t_cube_volume.refcount: referenced=1 → +1 (a node started referencing the
// volume), referenced=0 → -1 (a node stopped referencing it).
//
// Ref-count drift is not fatal to the sandbox lifecycle, so parse/DB errors are
// logged rather than returned.
func ApplyFromExtInfo(ctx context.Context, extInfo map[string][]byte) {
	if len(extInfo) == 0 {
		return
	}
	raw, ok := extInfo[ExtInfoKey]
	if !ok || len(raw) == 0 {
		return
	}
	var events []Event
	if err := json.Unmarshal(raw, &events); err != nil {
		log.G(ctx).Warnf("volume refcount: unmarshal events %q: %v", string(raw), err)
		return
	}
	for _, e := range events {
		if e.VolumeID == "" {
			continue
		}
		switch e.Referenced {
		case 1:
			Apply(ctx, e.VolumeID, 1)
		case 0:
			Apply(ctx, e.VolumeID, -1)
		default:
			log.G(ctx).Warnf("volume refcount: ignoring event with invalid referenced=%d volume=%s",
				e.Referenced, e.VolumeID)
		}
	}
}

// Apply atomically adds delta to the ref-count of the given volume. The value
// is clamped at zero so it can never go negative even if a stray decrement
// arrives (e.g. after a manual DB reset).
func Apply(ctx context.Context, volumeID string, delta int) {
	if delta == 0 {
		return
	}
	var expr interface{}
	if delta > 0 {
		expr = gorm.Expr("refcount + ?", delta)
	} else {
		expr = gorm.Expr("GREATEST(refcount - ?, 0)", -delta)
	}
	res := dao.Default().WithContext(ctx).
		Model(&models.VolumeRecord{}).
		Where("volume_id = ?", volumeID).
		Update("refcount", expr)
	if res.Error != nil {
		log.G(ctx).Warnf("volume refcount: apply delta=%d volume=%s: %v", delta, volumeID, res.Error)
		return
	}
	if res.RowsAffected == 0 {
		log.G(ctx).Warnf("volume refcount: volume %s not found while applying delta=%d", volumeID, delta)
	}
}
