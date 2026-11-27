// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/capability"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
)

// A repeated resolution for the same build must be answered from the node-wide
// cache: the second call cannot touch the filesystem, so a deleted artifact is
// invisible to it.
func TestResolveShimCapabilitiesServesRepeatsFromTheNodeCache(t *testing.T) {
	root := installShimArtifactForTest(t, "v0.9.9",
		map[string]uint32{capability.CapUpdateSnapshotCapture: capability.LevelInitial})

	cb := &cubeboxstore.CubeBox{Metadata: cubeboxstore.Metadata{ID: "sb-cache"}}
	pinShimArtifactForTest(cb, root, "v0.9.9")

	first := resolveShimCapabilitiesFor(context.Background(), cb)
	require.Equal(t, capability.SourceArtifact, first.Source)
	require.True(t, first.Capabilities.Has(capability.CapUpdateSnapshotCapture, capability.LevelInitial))

	// Remove the tree. A second read would now fall through to the frozen table
	// (or none), so an artifact answer proves the cache answered.
	require.NoError(t, os.RemoveAll(root))

	second := resolveShimCapabilitiesFor(context.Background(), cb)
	require.Equal(t, capability.SourceArtifact, second.Source,
		"the answer must come from the cache, not from a re-read")
	require.True(t, second.Capabilities.Has(capability.CapUpdateSnapshotCapture, capability.LevelInitial))
}

// The cache is shared across sandboxes: two sandboxes on one shim build resolve
// the same entry, which is the whole point of moving it off the per-sandbox record.
func TestNodeCacheIsSharedAcrossSandboxesOnOneBuild(t *testing.T) {
	root := installShimArtifactForTest(t, "v0.9.8",
		map[string]uint32{capability.CapUpdateSnapshotCapture: capability.LevelInitial})

	first := &cubeboxstore.CubeBox{Metadata: cubeboxstore.Metadata{ID: "sb-one"}}
	pinShimArtifactForTest(first, root, "v0.9.8")
	require.Equal(t, capability.SourceArtifact,
		resolveShimCapabilitiesFor(context.Background(), first).Source)

	// A second sandbox on the same build still resolves even with the artifact
	// gone, because the first sandbox already populated the shared entry.
	require.NoError(t, os.RemoveAll(root))
	second := &cubeboxstore.CubeBox{Metadata: cubeboxstore.Metadata{ID: "sb-two"}}
	pinShimArtifactForTest(second, root, "v0.9.8")

	require.Equal(t, capability.SourceArtifact,
		resolveShimCapabilitiesFor(context.Background(), second).Source)
}
