// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
)

func TestNormalizeSnapshotTypeAcceptsKnownValues(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", snapshotTypeFull},
		{"full", snapshotTypeFull},
		{"FULL", snapshotTypeFull},
		{" Full ", snapshotTypeFull},
		{"incremental", snapshotTypeIncremental},
		{"INCREMENTAL", snapshotTypeIncremental},
		{"soft-dirty", snapshotTypeSoftDirty},
		{"Soft-Dirty", snapshotTypeSoftDirty},
		{"SOFT-DIRTY", snapshotTypeSoftDirty},
		{" soft-dirty ", snapshotTypeSoftDirty},
		{"weird-value", snapshotTypeFull}, // unknown values must not silently break the CLI
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, normalizeSnapshotType(tc.in))
		})
	}
}

func TestBuildCubeRuntimeSnapshotArgsAlwaysIncludesSnapshotType(t *testing.T) {
	spec := &CubeboxSnapshotSpec{
		Resource: json.RawMessage(`{"cpu":1,"memory":1024}`),
	}

	t.Run("incremental for CommitSandbox", func(t *testing.T) {
		args := buildCubeRuntimeSnapshotArgs("sb-1", spec, "/tmp/s.tmp", "/dev/mapper/mem", snapshotTypeIncremental)
		// The flag must appear literally so cube-runtime takes the
		// pagemap_anon path; anything else would silently fall back to
		// full and produce a much larger snapshot.
		assert.Contains(t, args, "--snapshot-type")
		idx := indexOf(args, "--snapshot-type")
		require.GreaterOrEqual(t, idx, 0)
		assert.Equal(t, snapshotTypeIncremental, args[idx+1])
		assert.Contains(t, args, "--memory-vol")
	})

	t.Run("soft-dirty for CommitSandbox", func(t *testing.T) {
		args := buildCubeRuntimeSnapshotArgs("sb-1", spec, "/tmp/s.tmp", "/dev/mapper/mem", snapshotTypeSoftDirty)
		// Verbatim flag value -- the hypervisor's
		// SnapshotType::FromStr must see exactly "soft-dirty",
		// otherwise it falls back to incremental and the per-cycle
		// delta optimization is lost.
		assert.Contains(t, args, "--snapshot-type")
		idx := indexOf(args, "--snapshot-type")
		require.GreaterOrEqual(t, idx, 0)
		assert.Equal(t, snapshotTypeSoftDirty, args[idx+1])
		// memory-vol must still be present: the soft-dirty path
		// requires a destination file holding the previous cycle's
		// memory bytes (Cubelet reflink-clones it on the fast path).
		assert.Contains(t, args, "--memory-vol")
	})

	t.Run("full for AppSnapshot", func(t *testing.T) {
		args := buildCubeRuntimeSnapshotArgs("sb-1", spec, "/tmp/s.tmp", "/dev/mapper/mem", snapshotTypeFull)
		idx := indexOf(args, "--snapshot-type")
		require.GreaterOrEqual(t, idx, 0)
		assert.Equal(t, snapshotTypeFull, args[idx+1])
	})
}

func TestBuildCubeRuntimeSnapshotArgsOmitsMemoryVolWhenEmpty(t *testing.T) {
	args := buildCubeRuntimeSnapshotArgs("sb-1", nil, "/tmp/s.tmp", "", snapshotTypeFull)
	assert.NotContains(t, args, "--memory-vol")
}

func TestResolveBaseSnapshotIDPriority(t *testing.T) {
	t.Run("runtime label wins over annotations", func(t *testing.T) {
		cb := &cubeboxstore.CubeBox{
			Metadata: cubeboxstore.Metadata{
				Labels: map[string]string{
					constants.MasterAnnotationRuntimeSnapshotID: "rollback-snap",
				},
				Annotations: map[string]string{
					constants.MasterAnnotationRuntimeSnapshotID:     "create-time-snap",
					constants.MasterAnnotationAppSnapshotTemplateID: "tpl-original",
				},
			},
		}
		assert.Equal(t, "rollback-snap", resolveBaseSnapshotID(cb))
	})

	t.Run("create-time runtime annotation beats template id", func(t *testing.T) {
		cb := &cubeboxstore.CubeBox{
			Metadata: cubeboxstore.Metadata{
				Annotations: map[string]string{
					constants.MasterAnnotationRuntimeSnapshotID:     "create-time-snap",
					constants.MasterAnnotationAppSnapshotTemplateID: "tpl-original",
				},
			},
		}
		assert.Equal(t, "create-time-snap", resolveBaseSnapshotID(cb))
	})

	t.Run("falls back to template id", func(t *testing.T) {
		cb := &cubeboxstore.CubeBox{
			Metadata: cubeboxstore.Metadata{
				Annotations: map[string]string{
					constants.MasterAnnotationAppSnapshotTemplateID: "tpl-original",
				},
			},
		}
		assert.Equal(t, "tpl-original", resolveBaseSnapshotID(cb))
	})

	t.Run("returns empty when no binding", func(t *testing.T) {
		cb := &cubeboxstore.CubeBox{Metadata: cubeboxstore.Metadata{}}
		assert.Empty(t, resolveBaseSnapshotID(cb))
	})

	t.Run("nil cubebox is empty", func(t *testing.T) {
		assert.Empty(t, resolveBaseSnapshotID(nil))
	})
}

func TestResolveRollbackTargetsReturnsCatalogMemoryKind(t *testing.T) {
	// All-set request inputs win and the legacy contract (kind defaulted by
	// the storage layer) is preserved by returning an empty kind here.
	t.Run("all-set request returns empty kind for storage default", func(t *testing.T) {
		req := &cubebox.RollbackSandboxRequest{
			RootfsVol: "rfs",
			MemoryVol: "mem",
			MetaDir:   "/tmp/meta",
		}
		rfs, mem, kind, meta, err := resolveRollbackTargets(nil, req)
		require.NoError(t, err)
		assert.Equal(t, "rfs", rfs)
		assert.Equal(t, "mem", mem)
		assert.Empty(t, kind)
		assert.Equal(t, "/tmp/meta", meta)
	})

	// Partial input is rejected: it almost always indicates a master-side
	// bug, and silently filling in defaults would mask it.
	t.Run("partial request is rejected", func(t *testing.T) {
		req := &cubebox.RollbackSandboxRequest{RootfsVol: "rfs"}
		_, _, _, _, err := resolveRollbackTargets(nil, req)
		require.Error(t, err)
	})
}

func indexOf(s []string, target string) int {
	for i, v := range s {
		if v == target {
			return i
		}
	}
	return -1
}
