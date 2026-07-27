// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
)

// ListSandbox pages over RangeDBHost and treats an empty first page as "no
// queryable node exists" (see hasAnyDBHost in sandbox_list.go). That only holds
// because sortedNodesByClusters never keeps a node whose health has lapsed, so
// pin the invariant here: when the last node turns unhealthy the page empties.
func TestRangeDBHostDropsNodeWhoseHealthLapses(t *testing.T) {
	origSorted := l.sortedNodesByClusters
	defer func() {
		l.sortedNodesByClusters = origSorted
	}()

	n := &node.Node{InsID: "n1", IP: "10.0.0.1", Healthy: true}
	l.sortedNodesByClusters = map[string]node.NodeList{
		constants.DefaultInstanceTypeName: {n},
	}

	page, _ := RangeDBHost(1, 1, constants.DefaultInstanceTypeName)
	assert.Len(t, page, 1)

	n.Healthy = false
	l.updateSortedNodes(n)

	page, _ = RangeDBHost(1, 1, constants.DefaultInstanceTypeName)
	assert.Empty(t, page)
}
