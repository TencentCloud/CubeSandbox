// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package rlimit

import (
	"context"
	"testing"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenOpt(t *testing.T) {
	ctx := context.Background()
	nofile := uint64(10)
	spec := &specs.Spec{}
	opt := GenOpt(ctx, nofile)
	err := opt(ctx, nil, &containers.Container{}, spec)
	require.NoError(t, err)
	assert.Equal(t, []specs.POSIXRlimit{
		{
			Type: "RLIMIT_NOFILE",
			Hard: nofile,
			Soft: nofile,
		},
	}, spec.Process.Rlimits)
}

func TestGenOpt0(t *testing.T) {
	ctx := context.Background()
	spec := &specs.Spec{
		Process: &specs.Process{},
	}
	err := GenOpt(ctx, 0)(ctx, nil, &containers.Container{}, spec)
	require.NoError(t, err)
	assert.Equal(t, []specs.POSIXRlimit{
		{
			Type: "RLIMIT_NOFILE",
			Hard: DefaultNoFile,
			Soft: DefaultNoFile,
		},
	}, spec.Process.Rlimits)
}
