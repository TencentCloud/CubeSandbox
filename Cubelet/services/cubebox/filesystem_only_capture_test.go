// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"testing"

	"github.com/stretchr/testify/require"

	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/pkgs/snapshotmode"
)

func containerFor(id string, isPod bool) *cubeboxstore.Container {
	return &cubeboxstore.Container{
		Metadata: cubeboxstore.Metadata{ID: id},
		IsPod:    isPod,
	}
}

func TestValidateFilesystemOnlyCapture(t *testing.T) {
	single := &cubeboxstore.CubeBox{}
	single.AddContainer(containerFor("sb-1", true))

	t.Run("single_container_is_allowed", func(t *testing.T) {
		require.NoError(t, validateFilesystemOnlyCapture(single, snapshotmode.FsOnly))
	})

	multi := &cubeboxstore.CubeBox{}
	multi.AddContainer(containerFor("sb-1", true))
	multi.AddContainer(containerFor("c-2", false))

	t.Run("extra_container_is_refused_for_filesystem_only", func(t *testing.T) {
		err := validateFilesystemOnlyCapture(multi, snapshotmode.FsOnly)
		require.ErrorContains(t, err, snapshotmode.ErrFilesystemOnlyMultiContainer)
	})

	t.Run("full_snapshots_are_unaffected", func(t *testing.T) {
		require.NoError(t, validateFilesystemOnlyCapture(multi, snapshotmode.Full))
	})
}
