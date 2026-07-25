// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package nodemeta

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
)

func TestCountUserLabelsExcludesControlPlaneKey(t *testing.T) {
	assert.Equal(t, 0, countUserLabels(nil))
	assert.Equal(t, 1, countUserLabels(map[string]string{"env": "prod"}))
	assert.Equal(t, 1, countUserLabels(map[string]string{
		"env":                             "prod",
		constants.LabelSchedulingDisabled: constants.LabelSchedulingDisabledValue,
	}))
}

func TestStripAndPreserveSchedulingLabel(t *testing.T) {
	existing := map[string]string{
		"env":                             "old",
		constants.LabelSchedulingDisabled: constants.LabelSchedulingDisabledValue,
	}
	got := stripAndPreserveSchedulingLabel(existing, map[string]string{
		"env":                             "new",
		constants.LabelSchedulingDisabled: "false",
	})
	assert.Equal(t, "new", got["env"])
	assert.Equal(t, constants.LabelSchedulingDisabledValue, got[constants.LabelSchedulingDisabled])
}

func TestCloneSnapshotAndToSchedulerNodeCordon(t *testing.T) {
	snap := &NodeSnapshot{
		NodeID: "n1",
		HostIP: "10.0.0.1",
		Labels: map[string]string{
			constants.LabelSchedulingDisabled: constants.LabelSchedulingDisabledValue,
		},
		Healthy: true,
	}
	out := cloneSnapshot(snap)
	assert.True(t, out.SchedulingDisabled)

	n := toSchedulerNode(snap)
	assert.False(t, n.SchedulingAllowed())
	assert.True(t, n.SchedulingDisabled())
}

func TestSnapshotSchedulingDisabledCorrupt(t *testing.T) {
	assert.True(t, snapshotSchedulingDisabled(&NodeSnapshot{labelsJSONCorrupt: true}))
	assert.False(t, snapshotSchedulingDisabled(&NodeSnapshot{Labels: map[string]string{"env": "prod"}}))
}

func TestApplyReloadResultAppliesRemoteIsolation(t *testing.T) {
	s := newTestService(&NodeSnapshot{
		NodeID: "node-a",
		Labels: map[string]string{"env": "prod"},
	})
	s.applyReloadResult(map[string]*NodeSnapshot{
		"node-a": {
			NodeID: "node-a",
			Labels: map[string]string{
				"env":                             "prod",
				constants.LabelSchedulingDisabled: constants.LabelSchedulingDisabledValue,
			},
		},
	})
	s.mu.RLock()
	snap := s.nodes["node-a"]
	s.mu.RUnlock()
	assert.True(t, node.DecodeSchedulingDisabled(snap.Labels))
}
