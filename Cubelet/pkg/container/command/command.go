// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package command

import (
	"context"
	"fmt"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/oci"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	runtimespec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func WithProcessArgs(req *cubebox.ContainerConfig, image *imagespec.ImageConfig) oci.SpecOpts {
	return func(ctx context.Context, client oci.Client, c *containers.Container, s *runtimespec.Spec) (err error) {
		command, args := req.GetCommand(), req.GetArgs()

		if len(command) == 0 {

			if len(args) == 0 {
				args = append([]string{}, image.Cmd...)
			}
			if command == nil {
				if !(len(image.Entrypoint) == 1 && image.Entrypoint[0] == "") {
					command = append([]string{}, image.Entrypoint...)
				}
			}
		}
		if len(command) == 0 && len(args) == 0 {
			return fmt.Errorf("no command specified")
		}

		return oci.WithProcessArgs(append(command, args...)...)(ctx, client, c, s)
	}
}
