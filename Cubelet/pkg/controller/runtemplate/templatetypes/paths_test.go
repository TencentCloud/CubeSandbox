// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatetypes

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestComponentConstantsDoNotCollide(t *testing.T) {
	names := []string{
		CubeComponentCubeShim,
		CubeComponentCubeKernel,
		CubeComponentCubeImage,
		CubeComponentCubeAgent,
	}
	seen := map[string]struct{}{}
	for _, n := range names {
		assert.NotEmpty(t, n)
		_, dup := seen[n]
		assert.False(t, dup, "duplicate component name %q", n)
		seen[n] = struct{}{}
	}
	assert.Equal(t, "cube-shim", CubeComponentCubeShim)
	assert.Equal(t, "cube-kernel-scf", CubeComponentCubeKernel)
	assert.Equal(t, "cube-image", CubeComponentCubeImage)
	assert.Equal(t, "cube-agent", CubeComponentCubeAgent)
}

func TestDefaultRelativePathTable(t *testing.T) {
	cases := []struct {
		comp CubeComponent
		want string
	}{
		{CubeComponentCubeShim, RelativePathCubeShim},
		{CubeComponentCubeKernel, RelativePathCubeKernel},
		{CubeComponentCubeImage, RelativePathCubeImage},
		{CubeComponentCubeAgent, RelativePathCubeAgent},
		{"unknown", ""},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, DefaultRelativePath(tc.comp), tc.comp)
	}
}

func TestVersionedLocalPath(t *testing.T) {
	got := VersionedLocalPath(DefaultVersionedBaseDir, CubeComponentCubeShim, "1.2.3", RelativePathCubeShim)
	assert.Equal(t, "/data/cubelet/root/component_versions/cube-shim/1.2.3/bin/containerd-shim-cube-rs", got)
}

func TestInventoryVersionKey(t *testing.T) {
	full := "sha256:5df9ab5d8f8ce89a8115d47a9842ac9ed31b405cba679c7a1fae2ae70a8c3e62"
	// Digest after '@' wins.
	assert.Equal(t, "sha256-5df9ab5d8f8c", InventoryVersionKey("v0.6.0@"+full))
	assert.Equal(t, "sha256-5df9ab5d8f8c", InventoryVersionKey("sha256-5df9ab5d8f8c@"+full))
	assert.Equal(t, "sha256-5df9ab5d8f8c", InventoryVersionKey(full))
	// Non-digest component versions pass through.
	assert.Equal(t, "0.6.0-test1", InventoryVersionKey("0.6.0-test1"))
	assert.Equal(t, "", InventoryVersionKey(""))
	assert.Equal(t, "", InventoryVersionKey("@@@"))
	assert.Equal(t, "", InventoryVersionKey("sha256:short"))
	assert.Equal(t, "sha256-aaaaaaaaaaaa", InventoryVersionKey("v0.6.0@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	assert.Equal(t, "sha256-cccccccccccc@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		ContentAddressedKernelIdentity("sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"))
	assert.Equal(t, "", ContentAddressedKernelIdentity("unknown"))
	assert.Equal(t, "", ContentAddressedKernelIdentity(""))
}
