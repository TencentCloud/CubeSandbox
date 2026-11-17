// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubecow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
)

func TestPrepareCommitMemoryArtifactExpiryDuringClone(t *testing.T) {
	for _, restoreGone := range []bool{false, true} {
		t.Run(map[bool]string{false: "restore_base", true: "full"}[restoreGone], func(t *testing.T) {
			installCommitMemoryTestHooks(t)
			resolveBaseMemoryObjectFn = func(context.Context, *cubeboxstore.CubeBox, string) (*storage.CowSnapshotObject, error) {
				return &storage.CowSnapshotObject{Name: "previous"}, nil
			}
			resolveRestoreBaseMemoryObjectFn = func(context.Context, *cubeboxstore.CubeBox, string) (*storage.CowSnapshotObject, error) {
				return &storage.CowSnapshotObject{Name: "restore"}, nil
			}
			commitMemoryFromBaseFor = func(_ context.Context, _ string, source *storage.CowSnapshotObject, _ string, _ uint64) (*storage.CowSnapshotObject, error) {
				if source.Name == "previous" || restoreGone {
					return nil, &cubecow.CowError{Code: cubecow.SemNotFound}
				}
				return &storage.CowSnapshotObject{Name: "new"}, nil
			}
			getImportedSandboxMemoryFor = func(context.Context, string, string) (*storage.CowSnapshotObject, error) { return nil, nil }
			createMemoryVolumeFor = func(context.Context, string, string, uint64) (*storage.CowSnapshotObject, error) {
				return &storage.CowSnapshotObject{Name: "new-full"}, nil
			}
			obj, mode, err := prepareCommitMemoryArtifact(context.Background(), log.G(context.Background()), &cubeboxstore.CubeBox{}, "new", 4096, "xfs")
			require.NoError(t, err)
			require.NotNil(t, obj)
			if restoreGone {
				require.Equal(t, snapshotTypeFull, mode)
			} else {
				require.Equal(t, snapshotTypeIncremental, mode)
			}
		})
	}
}

func TestPrepareCommitMemoryArtifactDoesNotMaskCloneFailure(t *testing.T) {
	installCommitMemoryTestHooks(t)
	resolveBaseMemoryObjectFn = func(context.Context, *cubeboxstore.CubeBox, string) (*storage.CowSnapshotObject, error) {
		return &storage.CowSnapshotObject{Name: "base"}, nil
	}
	want := errors.New("disk full")
	commitMemoryFromBaseFor = func(context.Context, string, *storage.CowSnapshotObject, string, uint64) (*storage.CowSnapshotObject, error) {
		return nil, want
	}
	_, _, err := prepareCommitMemoryArtifact(context.Background(), log.G(context.Background()), &cubeboxstore.CubeBox{}, "new", 4096, "xfs")
	require.ErrorIs(t, err, want)
}
