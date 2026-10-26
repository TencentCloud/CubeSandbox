// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package types

import (
	"testing"

	"github.com/stretchr/testify/require"

	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestSortSandboxListPutsPausedLast(t *testing.T) {
	paused := int32(cubeboxv1.ContainerState_CONTAINER_PAUSED)
	running := int32(cubeboxv1.ContainerState_CONTAINER_RUNNING)
	items := []*SandboxBriefData{
		{SandboxID: "paused-old", Status: paused, CreateAt: 0, PauseAt: 10},
		{SandboxID: "run-old", Status: running, CreateAt: 1},
		{SandboxID: "run-new", Status: running, CreateAt: 3},
		{SandboxID: "paused-new", Status: paused, CreateAt: 2},
		{SandboxID: "exited", Status: int32(cubeboxv1.ContainerState_CONTAINER_EXITED), CreateAt: 2},
	}

	SortSandboxList(items)

	require.Equal(t, []string{"run-new", "exited", "run-old", "paused-new", "paused-old"}, ids(items))
}

func ids(items []*SandboxBriefData) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.SandboxID)
	}
	return out
}
