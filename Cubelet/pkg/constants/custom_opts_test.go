// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package constants

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRuntimeType(t *testing.T) {
	ctx := context.Background()
	rt := "io.containerd.cube.v2"

	ctx = WithRuntimeType(ctx, rt)
	assert.Equal(t, true, IsCubeRuntime(ctx))

	ctx = context.Background()
	rt = "io.container.runc.v2"
	ctx = WithRuntimeType(ctx, rt)
	assert.Equal(t, false, IsCubeRuntime(ctx))
}

func TestCubeRuntimeOption(t *testing.T) {
	ctx := context.Background()
	sandboxID := "test"
	ctx = WithCubeRuntimeOption(ctx, sandboxID)

	opt := GetCubeRuntimeOption(ctx)
	assert.NotNil(t, opt, "should get opt from ctx")
	assert.Equal(t, true, opt.PerPodShim)
	assert.Equal(t, sandboxID, opt.SandboxId)

	sandboxID2 := "test2"
	ctx = WithCubeRuntimeOption(ctx, sandboxID2)
	opt = GetCubeRuntimeOption(ctx)
	assert.NotNil(t, opt, "should get opt from ctx")
	assert.Equal(t, true, opt.PerPodShim)
	assert.Equal(t, sandboxID2, opt.SandboxId)
}
