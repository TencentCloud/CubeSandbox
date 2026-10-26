// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package rlimit

import (
	"context"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/opencontainers/runtime-spec/specs-go"
)

const DefaultNoFile = 1024

func GenOpt(ctx context.Context, nofile uint64) oci.SpecOpts {
	if nofile == 0 {
		nofile = DefaultNoFile
	}
	return withRLimits(nofile)
}

func withRLimits(nofile uint64) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *specs.Spec) error {
		if s.Process == nil {
			s.Process = &specs.Process{}
		}

		s.Process.Rlimits = []specs.POSIXRlimit{
			{
				Type: "RLIMIT_NOFILE",
				Hard: nofile,
				Soft: nofile,
			},
		}
		return nil
	}
}
