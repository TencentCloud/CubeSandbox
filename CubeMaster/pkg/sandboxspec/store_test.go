// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandboxspec

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gorm.io/gorm"
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

func TestCanonicalizeRequestPreservesMaskRequestHost(t *testing.T) {
	mask := "localhost:${PORT}"
	out, err := CanonicalizeRequest(&sandboxtypes.CreateCubeSandboxReq{
		InstanceType: "cubebox",
		CubeNetworkConfig: &sandboxtypes.CubeNetworkConfig{
			MaskRequestHost: &mask,
		},
	})
	require.NoError(t, err)
	require.NotNil(t, out.CubeNetworkConfig)
	require.NotNil(t, out.CubeNetworkConfig.MaskRequestHost)
	assert.Equal(t, mask, *out.CubeNetworkConfig.MaskRequestHost)
	assert.NotSame(t, &mask, out.CubeNetworkConfig.MaskRequestHost)
}

func TestGetAnnotationsReadsOnlyPersistedAnnotations(t *testing.T) {
	originalDB := getDB()
	t.Cleanup(func() {
		dbMu.Lock()
		db = originalDB
		dbMu.Unlock()
	})

	testDB, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "sandbox-spec.db")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, testDB.AutoMigrate(&models.SandboxSpec{}))
	require.NoError(t, Init(testDB))

	const networkQos = `{"Qos":{"BandWidth":{"Size":1250000,"RefillTime":100}},"Version":1}`
	require.NoError(t, testDB.Create(&models.SandboxSpec{
		SandboxID: "sb-qos",
		RequestJSON: `{"annotations":{"cube.master.net":` +
			`"{\"Qos\":{\"BandWidth\":{\"Size\":1250000,\"RefillTime\":100}},\"Version\":1}",` +
			`"unrelated":"keep"},"containers":"not-decoded-by-annotation-reader"}`,
	}).Error)

	annotations, err := GetAnnotations(context.Background(), "sb-qos")
	require.NoError(t, err)
	assert.Equal(t, networkQos, annotations[constants.CubeAnnotationsNetWork])
	assert.Equal(t, "keep", annotations["unrelated"])

	annotations["unrelated"] = "changed"
	again, err := GetAnnotations(context.Background(), "sb-qos")
	require.NoError(t, err)
	assert.Equal(t, "keep", again["unrelated"])
}
