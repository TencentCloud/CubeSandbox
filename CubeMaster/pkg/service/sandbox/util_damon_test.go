// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestFilterVMKernelCmdline(t *testing.T) {
	t.Run("allowlisted params pass", func(t *testing.T) {
		in := `["page_reporting.page_reporting_order=6","damon_reclaim.enabled=Y","damon_reclaim.monitor_region_start=4096"]`
		out, err := FilterVMKernelCmdline(in)
		require.NoError(t, err)
		assert.JSONEq(t, in, out)
	})

	t.Run("reject non-allowlisted module", func(t *testing.T) {
		_, err := FilterVMKernelCmdline(`["init=/bin/sh"]`)
		require.Error(t, err)
	})

	t.Run("reject embedded whitespace injection", func(t *testing.T) {
		_, err := FilterVMKernelCmdline(`["damon_reclaim.enabled=Y init=/bin/sh"]`)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not allowed")
	})

	t.Run("reject control characters", func(t *testing.T) {
		_, err := FilterVMKernelCmdline("[\"damon_reclaim.enabled=Y\\ninit=/bin/sh\"]")
		require.Error(t, err)
	})

	t.Run("reject shell metacharacters", func(t *testing.T) {
		_, err := FilterVMKernelCmdline(`["damon_reclaim.enabled=Y|init=/bin/sh"]`)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not allowed")
	})

	t.Run("reject oversized payload", func(t *testing.T) {
		entries := make([]string, 0, 100)
		for i := 0; i < 100; i++ {
			entries = append(entries, fmt.Sprintf("damon_reclaim.p%d=1", i))
		}
		payload, err := json.Marshal(entries)
		require.NoError(t, err)
		_, err = FilterVMKernelCmdline(string(payload))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exceeds")
	})

	t.Run("empty input", func(t *testing.T) {
		out, err := FilterVMKernelCmdline("")
		require.NoError(t, err)
		assert.Empty(t, out)
	})
}

func TestCheckAndGetContainerAnnotationFirstOnly(t *testing.T) {
	raw := `["damon_reclaim.enabled=Y"]`

	t.Run("first container keeps list", func(t *testing.T) {
		c := &cubebox.ContainerConfig{Annotations: map[string]string{}}
		cnt := &types.Container{
			Resources:   &types.Resource{Mem: "4096Mi"},
			Annotations: map[string]string{constants.CubeAnnotationVMKernelCmdlineAppend: raw},
		}
		// CubeMaster only validates and normalizes; the DAMON monitor region is
		// filled in Cubelet, where the worker arch and finalized RAM are known.
		require.NoError(t, checkAndGetContainerAnnotation(c, cnt, true))
		assert.JSONEq(t, raw, c.Annotations[constants.CubeAnnotationVMKernelCmdlineAppend])
	})

	t.Run("non-first container rejected", func(t *testing.T) {
		c := &cubebox.ContainerConfig{Annotations: map[string]string{}}
		cnt := &types.Container{
			Resources:   &types.Resource{Mem: "4096Mi"},
			Annotations: map[string]string{constants.CubeAnnotationVMKernelCmdlineAppend: raw},
		}
		err := checkAndGetContainerAnnotation(c, cnt, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must be set on the first container")
	})
}

func TestCheckAndGetAnnotationRejectsSandboxLevel(t *testing.T) {
	req := &types.CreateCubeSandboxReq{
		Annotations: map[string]string{
			constants.CubeAnnotationVMKernelCmdlineAppend: `["damon_reclaim.enabled=Y"]`,
		},
	}
	err := checkAndGetAnnotation(req, &cubebox.RunCubeSandboxRequest{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be set on the first container")
}
