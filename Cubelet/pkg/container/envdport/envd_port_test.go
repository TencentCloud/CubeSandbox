// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package envdport

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	cubebox "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
)

func TestAssign(t *testing.T) {
	tests := []struct {
		name       string
		containers []*cubebox.ContainerConfig
		wantPorts  []int
		wantErr    bool
	}{
		{
			name: "allocates distinct ports",
			containers: []*cubebox.ContainerConfig{
				{Name: "main"},
				{Name: "sidecar"},
				{Name: "worker"},
			},
			wantPorts: []int{49983, 49984, 49985},
		},
		{
			name: "preserves explicit sidecar port",
			containers: []*cubebox.ContainerConfig{
				{Name: "main"},
				{Name: "sidecar", Envs: []*cubebox.KeyValue{{Key: "ENVD_PORT", Value: "55000"}}},
			},
			wantPorts: []int{49983, 55000},
		},
		{
			name: "skips a reserved explicit sidecar port",
			containers: []*cubebox.ContainerConfig{
				{Name: "main"},
				{Name: "sidecar"},
				{Name: "worker", Envs: []*cubebox.KeyValue{{Key: "ENVD_PORT", Value: "49984"}}},
			},
			wantPorts: []int{49983, 49985, 49984},
		},
		{
			name: "rejects a non-default primary port",
			containers: []*cubebox.ContainerConfig{
				{Name: "main", Envs: []*cubebox.KeyValue{{Key: "ENVD_PORT", Value: "50000"}}},
			},
			wantErr: true,
		},
		{
			name: "rejects duplicate explicit ports",
			containers: []*cubebox.ContainerConfig{
				{Name: "main", Envs: []*cubebox.KeyValue{{Key: "ENVD_PORT", Value: "49983"}}},
				{Name: "sidecar", Envs: []*cubebox.KeyValue{{Key: "ENVD_PORT", Value: "49983"}}},
			},
			wantErr: true,
		},
		{
			name: "rejects invalid port",
			containers: []*cubebox.ContainerConfig{
				{Name: "main", Envs: []*cubebox.KeyValue{{Key: "ENVD_PORT", Value: "not-a-port"}}},
			},
			wantErr: true,
		},
		{
			name: "rejects nil container",
			containers: []*cubebox.ContainerConfig{
				nil,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Assign(tt.containers)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Len(t, tt.containers, len(tt.wantPorts))
			for i, container := range tt.containers {
				port, ok := Get(container)
				require.True(t, ok)
				assert.Equal(t, tt.wantPorts[i], port)
			}
		})
	}
}

func TestPrepareRequestAddsEveryEnvdPortOnce(t *testing.T) {
	req := &cubebox.RunCubeSandboxRequest{
		Containers: []*cubebox.ContainerConfig{
			{Name: "main"},
			{Name: "sidecar"},
		},
		ExposedPorts: []int64{8080, 49983},
	}
	require.NoError(t, PrepareRequest(req))
	assert.Equal(t, []int64{8080, 49983, 49984}, req.ExposedPorts)

	// Preparation is idempotent when request setup is repeated.
	require.NoError(t, PrepareRequest(req))
	assert.Equal(t, []int64{8080, 49983, 49984}, req.ExposedPorts)
}
