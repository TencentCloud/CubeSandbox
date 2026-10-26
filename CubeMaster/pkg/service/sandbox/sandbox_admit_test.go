// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/ret"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
)

func TestAdmitSelectedHost(t *testing.T) {
	mk := func(id string, disabled bool) *node.Node {
		n := &node.Node{InsID: id, IP: "10.0.0.1"}
		n.SetSchedulingDisabled(disabled)
		return n
	}
	cacheOf := func(nodes ...*node.Node) func(string) (*node.Node, bool) {
		m := make(map[string]*node.Node, len(nodes))
		for _, n := range nodes {
			m[n.ID()] = n
		}
		return func(id string) (*node.Node, bool) {
			n, ok := m[id]
			return n, ok
		}
	}

	t.Run("nil host", func(t *testing.T) {
		got, err := admitSelectedHost(nil, cacheOf())
		assert.Nil(t, got)
		assertAdmitFailed(t, err, "no selected host")
	})

	t.Run("cache miss", func(t *testing.T) {
		got, err := admitSelectedHost(mk("missing", false), cacheOf())
		assert.Nil(t, got)
		assertAdmitFailed(t, err, "missing from cache")
	})

	t.Run("cordoned", func(t *testing.T) {
		host := mk("n1", true)
		got, err := admitSelectedHost(mk("n1", false), cacheOf(host))
		assert.Nil(t, got)
		assertAdmitFailed(t, err, "scheduling-disabled")
	})

	t.Run("schedulable refreshes host", func(t *testing.T) {
		fresh := mk("n1", false)
		fresh.IP = "10.0.0.9"
		selected := mk("n1", false)
		selected.IP = "10.0.0.1"

		got, err := admitSelectedHost(selected, cacheOf(fresh))
		assert.NoError(t, err)
		assert.Same(t, fresh, got)
		assert.Equal(t, "10.0.0.9", got.HostIP())
	})
}

func TestRefreshAndAdmitHostUsesLocalcache(t *testing.T) {
	c := &createSandboxContext{}
	err := c.refreshAndAdmitHost()
	assertAdmitFailed(t, err, "no selected host")
}

func assertAdmitFailed(t *testing.T, err error, msgSubstr string) {
	t.Helper()
	assert.Error(t, err)
	status, ok := ret.FromError(err)
	assert.True(t, ok)
	assert.Equal(t, errorcode.ErrorCode_SelectNodesFailed, status.Code())
	assert.True(t, strings.Contains(status.Message(), msgSubstr), "msg=%q want substr %q", status.Message(), msgSubstr)
}
