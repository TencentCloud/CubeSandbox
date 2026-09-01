// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package container

import (
	"context"
	"path/filepath"

	"github.com/containerd/containerd/v2/contrib/seccomp"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func GenOpt(ctx context.Context, c *cubebox.ContainerConfig) []oci.SpecOpts {
	var opt []oci.SpecOpts
	opt = append(opt,
		oci.WithDefaultSpec(),
		WithUMount,
		oci.WithDefaultUnixDevices,
		seccomp.WithDefaultProfile(),

		oci.WithNewPrivileges,
	)
	return opt
}

func WithUMount(_ context.Context, _ oci.Client, c *containers.Container, s *specs.Spec) error {
	var (
		mounts  []specs.Mount
		current = s.Mounts
	)
	for _, m := range current {
		if filepath.Clean(m.Destination) == "/run" {
			continue
		}
		if m.Destination == "/dev/shm" {
			continue
		}
		mounts = append(mounts, m)
	}
	s.Mounts = mounts
	return nil
}
