// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package env

import (
	"context"

	"github.com/containerd/containerd/v2/pkg/oci"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func GenOpt(ctx context.Context, c *cubebox.ContainerConfig, image *imagespec.ImageConfig) []oci.SpecOpts {
	opt := []oci.SpecOpts{
		oci.WithDefaultPathEnv,
	}

	envList := append([]string{}, image.Env...)
	var cEnvList []string
	for _, env := range c.Envs {
		cEnvList = append(cEnvList, env.Key+"="+env.Value)
	}

	return append(opt, oci.WithEnv(envList), oci.WithEnv(cEnvList))
}
