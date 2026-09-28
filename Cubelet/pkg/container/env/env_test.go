// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package env

import (
	"context"
	"testing"

	"github.com/containerd/containerd/v2/pkg/oci"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"

	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestGenOpt(t *testing.T) {

	envFromImage := []string{
		"IMAGE_ENV1=value1",
		"IMAGE_ENV2=value2",
	}
	image := &imagespec.ImageConfig{
		Env: envFromImage,
	}

	defaultEnv := "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

	cases := []struct {
		name           string
		c              *cubebox.ContainerConfig
		expectedEnvs   []string
		expectedErrMsg string
	}{
		{
			name: "bothEnvsEmpty",
			c:    &cubebox.ContainerConfig{},
			expectedEnvs: []string{
				defaultEnv,
				"IMAGE_ENV1=value1",
				"IMAGE_ENV2=value2",
			},
			expectedErrMsg: "",
		},
		{
			name: "envsFromImageWithEmptyContainerEnvs",
			c:    &cubebox.ContainerConfig{Envs: nil},
			expectedEnvs: []string{
				defaultEnv,
				"IMAGE_ENV1=value1",
				"IMAGE_ENV2=value2",
			},
			expectedErrMsg: "",
		},
		{
			name: "envsFromImageAndContainer",
			c: &cubebox.ContainerConfig{
				Envs: []*cubebox.KeyValue{
					{Key: "ENV1", Value: "value1"},
					{Key: "IMAGE_ENV1", Value: "value1_override"},
				},
			},
			expectedEnvs: []string{
				defaultEnv,
				"IMAGE_ENV1=value1_override",
				"IMAGE_ENV2=value2",
				"ENV1=value1",
			},
			expectedErrMsg: "",
		},
	}

	ctx := context.Background()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := GenOpt(ctx, tc.c, image)

			spec := &oci.Spec{
				Process: &specs.Process{},
			}

			assert.NoError(t, oci.Compose(opts...)(nil, nil, nil, spec))

			assert.Equal(t, tc.expectedEnvs, spec.Process.Env)
		})
	}
}
