// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package nodemeta

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
)

func TestSchedulingDisabledFromLabelsRequiresCanonicalValue(t *testing.T) {
	assert.False(t, schedulingDisabledFromLabels(nil))
	assert.False(t, schedulingDisabledFromLabels(map[string]string{
		constants.LabelSchedulingDisabled: "false",
	}))
	assert.True(t, schedulingDisabledFromLabels(map[string]string{
		constants.LabelSchedulingDisabled: constants.LabelSchedulingDisabledValue,
	}))
}

func TestApplyReloadResultEvictsMissingNode(t *testing.T) {
	s := newTestService(
		&NodeSnapshot{NodeID: "keep"},
		&NodeSnapshot{NodeID: "deleted"},
	)
	s.applyReloadResult(map[string]*NodeSnapshot{"keep": {NodeID: "keep"}})

	s.mu.RLock()
	defer s.mu.RUnlock()
	_, kept := s.nodes["keep"]
	_, deleted := s.nodes["deleted"]
	assert.True(t, kept)
	assert.False(t, deleted)
}

func TestApplyReloadResultDoesNotEvictReregisteredNode(t *testing.T) {
	s := newTestService(
		&NodeSnapshot{NodeID: "keep"},
		&NodeSnapshot{NodeID: "reregistered"},
	)
	s.ready = true
	origSync := syncNodeHealthFn
	origEvict := evictNodeFn
	defer func() {
		syncNodeHealthFn = origSync
		evictNodeFn = origEvict
	}()

	syncNodeHealthFn = func(*NodeSnapshot) {
		s.mu.Lock()
		s.nodes["reregistered"] = &NodeSnapshot{NodeID: "reregistered"}
		s.mu.Unlock()
	}
	var evicted []string
	evictNodeFn = func(nodeID string) {
		evicted = append(evicted, nodeID)
	}

	s.applyReloadResult(map[string]*NodeSnapshot{"keep": {NodeID: "keep"}})

	s.mu.RLock()
	_, registered := s.nodes["reregistered"]
	s.mu.RUnlock()
	assert.True(t, registered)
	assert.NotContains(t, evicted, "reregistered")
}
