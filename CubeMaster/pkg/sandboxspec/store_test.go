// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandboxspec

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

// TestCanonicalizeRequestStripsTransientSnapshotAnnotations locks in the v4+
// contract: per-invocation runtime-snapshot binding annotations are scrubbed
// from the canonicalized request so they cannot bleed into later snapshots
// of the same sandbox or stored template requests. (v5 removed the physical
// memory_vol/memory_kind annotations entirely; only logical id +
// attached_at remain to be stripped.)
func TestCanonicalizeRequestStripsTransientSnapshotAnnotations(t *testing.T) {
	req := &sandboxtypes.CreateCubeSandboxReq{
		InstanceType: "cubebox",
		Annotations: map[string]string{
			constants.CubeAnnotationRuntimeSnapshotID:         "snap-1",
			constants.CubeAnnotationRuntimeSnapshotAttachedAt: "2026-05-01T00:00:00Z",
			constants.CubeAnnotationAppSnapshotTemplateID:     "tpl-keep",
			"unrelated-annotation":                            "preserve",
		},
		Labels:  map[string]string{"keep": "me"},
		Timeout: sandboxtypes.TimeoutPtr(30),
	}

	out, err := CanonicalizeRequest(req)
	require.NoError(t, err)

	for _, k := range []string{
		constants.CubeAnnotationRuntimeSnapshotID,
		constants.CubeAnnotationRuntimeSnapshotAttachedAt,
	} {
		_, present := out.Annotations[k]
		assert.Falsef(t, present, "annotation %q must be stripped after canonicalize", k)
	}

	assert.Equal(t, "tpl-keep", out.Annotations[constants.CubeAnnotationAppSnapshotTemplateID],
		"logical template id must be preserved (long-term provenance)")
	assert.Equal(t, "preserve", out.Annotations["unrelated-annotation"],
		"unrelated annotations must be preserved")
	assert.Equal(t, "me", out.Labels["keep"])
	assert.Zero(t, out.Timeout)
}

// TestCanonicalizeRequestHandlesNilAnnotations confirms nil maps are forced
// to empty maps for stable JSON encoding even when there is nothing to strip.
func TestCanonicalizeRequestHandlesNilAnnotations(t *testing.T) {
	out, err := CanonicalizeRequest(&sandboxtypes.CreateCubeSandboxReq{InstanceType: "cubebox"})
	require.NoError(t, err)
	require.NotNil(t, out.Annotations)
	require.NotNil(t, out.Labels)
	assert.Empty(t, out.Annotations)
	assert.Empty(t, out.Labels)
}
