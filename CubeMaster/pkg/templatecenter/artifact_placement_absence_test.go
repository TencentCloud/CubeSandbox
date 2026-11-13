// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
)

// TestPlacementNodeAbsentLongEnoughGraceBoundary pins the grace semantics
// without a database: the boundary is inclusive and legacy ip: ids never
// qualify, however long they have been "absent".
func TestPlacementNodeAbsentLongEnoughGraceBoundary(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	var since time.Time
	var ok bool
	patches.ApplyFunc(localcache.AbsentSince, func(string) (time.Time, bool) { return since, ok })

	now := time.Now()
	cases := []struct {
		name   string
		nodeID string
		since  time.Time
		ok     bool
		want   bool
	}{
		{"absent past grace", "node-1", now.Add(-placementAbsenceGrace - time.Second), true, true},
		{"absent exactly the grace period", "node-1", now.Add(-placementAbsenceGrace), true, true},
		{"absent inside grace", "node-1", now.Add(-placementAbsenceGrace + time.Minute), true, false},
		{"present or not yet synced", "node-1", time.Time{}, false, false},
		{"legacy ip id never qualifies", "ip:10.0.0.9", now.Add(-100 * time.Hour), true, false},
		{"empty node id", "", now.Add(-100 * time.Hour), true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			since, ok = c.since, c.ok
			p := &models.ArtifactNodePlacement{NodeID: c.nodeID}
			if got := placementNodeAbsentLongEnough(p, now); got != c.want {
				t.Fatalf("placementNodeAbsentLongEnough(%q)=%v want %v", c.nodeID, got, c.want)
			}
		})
	}
	if placementNodeAbsentLongEnough(nil, now) {
		t.Fatal("nil placement must not be treated as absent")
	}
}
