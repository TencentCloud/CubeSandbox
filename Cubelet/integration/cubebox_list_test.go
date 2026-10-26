// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package integration

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestCubeboxSimpleList(t *testing.T) {
	t.Logf("Create two cubebox and list them")
	cb1 := StandardCubeboxConfigWithCleanup(t)
	cb2 := StandardCubeboxConfigWithCleanup(t)

	list := ListCubebox(t, &cubebox.ListCubeSandboxRequest{})
	require.LessOrEqual(t, 2, len(list), "expect at least 2 cubebox")

	idList := make([]string, 0, len(list))
	for _, cb := range list {
		idList = append(idList, cb.GetId())
	}

	require.Contains(t, idList, cb1)
	require.Contains(t, idList, cb2)

	for _, cb := range list {
		require.Len(t, cb.GetContainers(), 2)
	}
}

func TestCubeboxListByID(t *testing.T) {
	t.Logf("Create a cubebox and list by id")
	cb := StandardCubeboxConfigWithCleanup(t)

	list := ListCubebox(t, &cubebox.ListCubeSandboxRequest{
		Id: &cb,
	})
	require.Len(t, list, 1)

	got := list[0]

	assert.Equal(t, cb, got.GetId())
	assert.Len(t, got.GetContainers(), 2)
}

func TestCubeboxListByLabel(t *testing.T) {
	t.Logf("Create a cubebox and list by label")
	StandardCubeboxConfigWithCleanup(t)

	list := ListCubebox(t, &cubebox.ListCubeSandboxRequest{
		Filter: &cubebox.CubeSandboxFilter{
			LabelSelector: map[string]string{
				"io.kubernetes.cri.container-type": "sandbox",
			},
		},
	})

	for _, got := range list {
		require.Len(t, got.GetContainers(), 1)
		assert.Equal(t, "sandbox", got.GetContainers()[0].Type)
	}

	list = ListCubebox(t, &cubebox.ListCubeSandboxRequest{
		Filter: &cubebox.CubeSandboxFilter{
			LabelSelector: map[string]string{
				"io.kubernetes.cri.container-type": "container",
			},
		},
	})

	for _, got := range list {
		require.Len(t, got.GetContainers(), 1)
		assert.Equal(t, "container", got.GetContainers()[0].Type)
	}
}

func TestCubeboxListByState(t *testing.T) {
	t.Logf("Create a cubebox and list by state")
	cb := StandardCubeboxConfigWithCleanup(t)

	list := ListCubebox(t, &cubebox.ListCubeSandboxRequest{
		Filter: &cubebox.CubeSandboxFilter{
			State: &cubebox.ContainerStateValue{
				State: cubebox.ContainerState_CONTAINER_RUNNING,
			},
		},
	})

	require.LessOrEqual(t, 1, len(list))
	find := false
	for _, got := range list {
		if got.GetId() == cb {
			find = true

			for _, c := range got.GetContainers() {
				assert.Equal(t, cubebox.ContainerState_CONTAINER_RUNNING, c.GetState())
			}
		}
	}
	require.Truef(t, find, "not found cb %q in list", cb)

	list = ListCubebox(t, &cubebox.ListCubeSandboxRequest{
		Filter: &cubebox.CubeSandboxFilter{
			State: &cubebox.ContainerStateValue{
				State: cubebox.ContainerState_CONTAINER_UNKNOWN,
			},
		},
	})
	require.Len(t, list, 0)
}
