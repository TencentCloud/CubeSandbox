// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"strings"
	"testing"
	"time"
)

func TestLifecycleMetadataAndCapture(t *testing.T) {
	for _, labels := range []map[string]string{nil, {}, {"api_token": "user", "cube.master.auth": "credential", "host-mount": "private", "X-Caller": "internal"}} {
		m := UserMetadata(labels)
		req := &types.CreateCubeSandboxReq{WebhookMetadata: m}
		_, f := PrepareLifecycle(req)
		c, err := Capture(CaptureInput{ID: uuid.New(), Type: TypeCreated, Origin: OriginLifecycle, SandboxID: "sb", OccurredAt: time.Now(), Fields: f})
		require.NoError(t, err)
		if f.Metadata != nil {
			f.Metadata["post_capture"] = "changed"
		}
		if labels != nil {
			labels["api_token"] = "changed"
		}
		p, err := MapToProtocol(c)
		require.NoError(t, err)
		if m.Values == nil {
			require.Nil(t, p.SandboxMetadata)
		} else {
			require.NotNil(t, p.SandboxMetadata)
			require.NotEqual(t, "changed", p.SandboxMetadata["api_token"])
		}
		require.NotContains(t, p.SandboxMetadata, "post_capture")
		require.NotContains(t, p.SandboxMetadata, "host-mount")
		require.NotContains(t, p.SandboxMetadata, "cube.master.auth")
	}
	_, unknown := PrepareLifecycle(&types.CreateCubeSandboxReq{Labels: map[string]string{"unknown": "secret"}})
	require.Nil(t, unknown.Metadata)
	for _, m := range []map[string]string{{strings.Repeat("k", 257): "v"}, {"k": strings.Repeat("v", 4097)}, {"a": strings.Repeat("v", 4096), "b": strings.Repeat("v", 4096), "c": strings.Repeat("v", 4096), "d": strings.Repeat("v", 4096)}} {
		require.True(t, UserMetadata(m).TooLarge)
	}
	m := map[string]string{}
	for i := 0; i < 65; i++ {
		m[string(rune(i))] = "x"
	}
	require.True(t, UserMetadata(m).TooLarge)
}
func TestLifecycleResources(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		mem  bool
		want int64
	}{{"1", false, 1}, {"1000m", false, 1}, {"1500m", false, 0}, {"-1", false, 0}, {"0", false, 0}, {"bad", false, 0}, {"9223372036854775808", false, 0}, {"1Gi", true, 1024}, {"512Mi", true, 512}, {"512M", true, 488}, {"1.5Mi", true, 1}, {"-1Gi", true, 0}, {"999999999999999999999Gi", true, 0}} {
		got := quantityInteger(tc.raw, tc.mem)
		if tc.want == 0 {
			require.Nil(t, got, tc.raw)
		} else {
			require.NotNil(t, got, tc.raw)
			require.Equal(t, tc.want, *got, tc.raw)
		}
	}
	req := &types.CreateCubeSandboxReq{InstanceType: "cubebox", Containers: []*types.Container{{Resources: &types.Resource{Cpu: "1500m", Mem: "1Gi"}}}}
	_, f := PrepareLifecycle(req)
	f.PauseMode = "full"
	c, e := Capture(CaptureInput{ID: uuid.New(), Type: TypePaused, Origin: OriginLifecycle, SandboxID: "sb", OccurredAt: time.Now(), Fields: f})
	require.NoError(t, e)
	p, e := MapToProtocol(c)
	require.NoError(t, e)
	require.Nil(t, p.Execution.VCPUCount)
	require.Equal(t, int64(1024), *p.Execution.MemoryMB)
	require.Empty(t, p.Execution.StartedAt)
	require.Nil(t, p.Execution.ExecutionTimeMS)
	req.Containers = append(req.Containers, req.Containers[0])
	_, f = PrepareLifecycle(req)
	require.Empty(t, f.CPU)
	require.Empty(t, f.Memory)
}
