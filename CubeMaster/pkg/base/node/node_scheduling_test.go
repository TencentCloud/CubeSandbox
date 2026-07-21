// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package node

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
)

func TestDecodeSchedulingDisabled(t *testing.T) {
	assert.False(t, DecodeSchedulingDisabled(nil))
	assert.False(t, DecodeSchedulingDisabled(map[string]string{}))
	assert.False(t, DecodeSchedulingDisabled(map[string]string{"env": "prod"}))
	assert.True(t, DecodeSchedulingDisabled(map[string]string{
		constants.LabelSchedulingDisabled: constants.LabelSchedulingDisabledValue,
	}))
	assert.True(t, DecodeSchedulingDisabled(map[string]string{
		constants.LabelSchedulingDisabled: "false",
	}))
}

func TestNodeSchedulingDisabledZeroValue(t *testing.T) {
	n := &Node{}
	assert.True(t, n.SchedulingAllowed())
	assert.False(t, n.SchedulingDisabled())
}

func TestNodeSetSchedulingDisabledAndClone(t *testing.T) {
	n := &Node{InsID: "n1"}
	n.SetSchedulingDisabled(true)
	assert.False(t, n.SchedulingAllowed())
	assert.True(t, n.SchedulingDisabled())

	cloned := n.Clone()
	assert.False(t, cloned.SchedulingAllowed())
	assert.True(t, cloned.SchedulingDisabled())

	cloned.SetSchedulingDisabled(false)
	assert.False(t, n.SchedulingAllowed())
	assert.True(t, cloned.SchedulingAllowed())
}

func TestNodeSchedulingDisabledJSONRoundTrip(t *testing.T) {
	n := &Node{InsID: "n1", IP: "10.0.0.1", Healthy: true}
	n.SetSchedulingDisabled(true)

	raw, err := json.Marshal(n)
	assert.NoError(t, err)
	assert.Contains(t, string(raw), `"SchedulingDisabled":true`)

	var got Node
	assert.NoError(t, json.Unmarshal(raw, &got))
	assert.True(t, got.SchedulingDisabled())
	assert.False(t, got.SchedulingAllowed())
	assert.Equal(t, "n1", got.InsID)
}
