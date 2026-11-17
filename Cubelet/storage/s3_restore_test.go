// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubecow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

// A filesystem-only package has no memory object. When the catalog cannot be
// read, the naming fallback must not invent one: a phantom memory ref makes
// activation fail on a package that is complete.
func TestActivateObjectRefsSkipsAbsentMemoryObject(t *testing.T) {
	useS3CatalogRoot(t, t.TempDir())
	useTestCowStorage(t, &fakeCowEngine{volumeInfos: map[string]*cubecow.Volume{
		"tpl-snap-fs-rootfs": {},
	}})

	refs, err := activateObjectRefs(context.Background(), mustS3Cow(t), cow.BackendS3, "snap-fs")
	require.NoError(t, err)
	require.Equal(t, "rootfs", rolesOf(refs)[0])
	require.NotContains(t, rolesOf(refs), "memory")
}

func TestActivateObjectRefsKeepsExistingMemoryObject(t *testing.T) {
	useS3CatalogRoot(t, t.TempDir())
	useTestCowStorage(t, &fakeCowEngine{volumeInfos: map[string]*cubecow.Volume{
		"tpl-snap-full-rootfs": {},
		"tpl-snap-full-memory": {},
	}})

	refs, err := activateObjectRefs(context.Background(), mustS3Cow(t), cow.BackendS3, "snap-full")
	require.NoError(t, err)
	require.Equal(t, []string{"rootfs", "memory"}, rolesOf(refs)[:2])
	require.Equal(t, "tpl-snap-full-memory", refs[1].Name)
}

// A readable catalog is authoritative: the filesystem-only entry has an empty
// memory_vol even if some stale object happens to carry the conventional name.
func TestActivateObjectRefsPrefersCatalogOverConvention(t *testing.T) {
	root := t.TempDir()
	useS3CatalogRoot(t, root)
	useTestCowStorage(t, &fakeCowEngine{volumeInfos: map[string]*cubecow.Volume{
		"tpl-snap-fs-rootfs": {},
		"tpl-snap-fs-memory": {},
	}})
	entry := &SnapshotCatalogEntry{
		SnapshotID:   "snap-fs",
		SnapshotPath: filepath.Join(root, "snap-fs"),
		RootfsVol:    "tpl-snap-fs-rootfs",
		RootfsKind:   cowKindSnapshot,
		Backend:      cow.BackendS3,
	}
	require.NoError(t, WriteSnapshotCatalogFor(cow.BackendS3, entry))

	refs, err := activateObjectRefs(context.Background(), mustS3Cow(t), cow.BackendS3, "snap-fs")
	require.NoError(t, err)
	require.Equal(t, "rootfs", rolesOf(refs)[0])
	require.NotContains(t, rolesOf(refs), "memory")
}

func rolesOf(refs []CowObjectRef) []string {
	roles := make([]string, 0, len(refs))
	for _, ref := range refs {
		roles = append(roles, ref.Role)
	}
	return roles
}

func mustS3Cow(t *testing.T) *S3Cow {
	t.Helper()
	store, err := requireS3Cow()
	require.NoError(t, err)
	return store
}

// A sealed package stores memory under "<name>-snap"; the fallback must find
// it, otherwise a full package would be exported/activated as rootfs-only.
func TestActivateObjectRefsFindsSealedMemoryObject(t *testing.T) {
	useS3CatalogRoot(t, t.TempDir())
	useTestCowStorage(t, &fakeCowEngine{volumeInfos: map[string]*cubecow.Volume{
		"tpl-snap-full-rootfs":      {},
		"tpl-snap-full-memory-snap": {},
	}})

	refs, err := activateObjectRefs(context.Background(), mustS3Cow(t), cow.BackendS3, "snap-full")
	require.NoError(t, err)
	require.Equal(t, []string{"rootfs", "memory"}, rolesOf(refs)[:2])
	require.Equal(t, "tpl-snap-full-memory-snap", refs[1].Name)
	require.Equal(t, cowKindSnapshot, refs[1].Kind)
}

// A storage outage must not be reported as "the package has no rootfs object":
// the caller would treat a recoverable failure as a corrupt artifact.
func TestActivateObjectRefsPropagatesProbeFailures(t *testing.T) {
	useS3CatalogRoot(t, t.TempDir())
	useTestCowStorage(t, &fakeCowEngine{
		volumeInfoErrs: map[string]error{
			"tpl-snap-full-rootfs": errors.New("s3lvol: transport closed"),
		},
	})

	_, err := activateObjectRefs(context.Background(), mustS3Cow(t), cow.BackendS3, "snap-full")
	require.Error(t, err)
	require.Contains(t, err.Error(), "transport closed")
	require.NotErrorIs(t, err, ErrCowObjectMissing)
}

// A package is identified by its rootfs. Memory or metadata left behind by a
// partially cleaned package must not make Activate or Upload look successful.
func TestActivateRejectsAPackageWithoutRootfs(t *testing.T) {
	useS3CatalogRoot(t, t.TempDir())
	useTestCowStorage(t, &fakeCowEngine{volumeInfos: map[string]*cubecow.Volume{
		"tpl-snap-torn-memory":      {},
		"tpl-snap-torn-memory-snap": {},
	}})

	err := ActivateSnapshot(context.Background(), cow.BackendS3, "snap-torn")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrCowObjectMissing)
}
