// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cgroup

import (
	"context"
	"testing"

	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/stretchr/testify/assert"

	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestGenOpt(t *testing.T) {

	cases := []struct {
		name           string
		container      *cubebox.ContainerConfig
		expectedOpts   []oci.SpecOpts
		expectedErrMsg string
	}{
		{
			name: "withMemoryLimit",
			container: &cubebox.ContainerConfig{
				Resources: &cubebox.Resource{
					Mem: "256Mi",
				},
			},
			expectedOpts:   []oci.SpecOpts{oci.WithMemoryLimit(256 * 1024 * 1024)},
			expectedErrMsg: "",
		},
		{
			name: "withInvalidMemoryLimit",
			container: &cubebox.ContainerConfig{
				Resources: &cubebox.Resource{
					Mem: "invalid",
				},
			},
			expectedOpts:   []oci.SpecOpts{},
			expectedErrMsg: "quantities must match the regular expression",
		},
		{
			name:           "withoutResources",
			container:      &cubebox.ContainerConfig{},
			expectedOpts:   []oci.SpecOpts{},
			expectedErrMsg: "",
		},
	}

	ctx := context.Background()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {

			opts, err := GenOpt(ctx, tc.container)

			if err != nil {
				assert.Contains(t, err.Error(), tc.expectedErrMsg)
				return
			}

			spec := &oci.Spec{}
			expected := &oci.Spec{}

			assert.NoError(t, oci.Compose(opts...)(nil, nil, nil, spec))
			assert.NoError(t, oci.Compose(tc.expectedOpts...)(nil, nil, nil, expected))

			assert.Equal(t, expected, spec)
		})
	}
}
