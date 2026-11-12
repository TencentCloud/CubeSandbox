// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
)

func TestSetRuntimeSnapshotBinding(t *testing.T) {
	cb := &cubeboxstore.CubeBox{}

	setRuntimeSnapshotBinding(cb, "snap-1")

	if got := cb.RuntimeBaseID(); got != "snap-1" {
		t.Fatalf("runtime snapshot id = %q, want snap-1", got)
	}
}

func TestSetRuntimeSnapshotBindingSkipsEmptyID(t *testing.T) {
	cb := &cubeboxstore.CubeBox{}
	setRuntimeSnapshotBinding(cb, "")
	if cb.RuntimeBaseID() != "" {
		t.Fatalf("empty snapshot id must not bind a base, got %q", cb.RuntimeBaseID())
	}
}

func TestPersistRuntimeSnapshotBindingUpdatesDurableBaseline(t *testing.T) {
	cb := &cubeboxstore.CubeBox{Metadata: cubeboxstore.Metadata{ID: "sandbox"}}
	setRuntimeSnapshotBinding(cb, "previous")
	syncer := &guestMetricsEpochSyncerFake{}

	require.NoError(t, persistRuntimeSnapshotBinding(context.Background(), syncer, cb, "next"))
	require.Equal(t, []string{"sandbox"}, syncer.syncIDs)
	require.Equal(t, "next", cb.RuntimeBaseID())
}

func TestPersistRuntimeSnapshotBindingRestoresLineageOnFailure(t *testing.T) {
	cb := &cubeboxstore.CubeBox{Metadata: cubeboxstore.Metadata{ID: "sandbox"}}
	cb.BindRestoreBase("previous-restore")
	setRuntimeSnapshotBinding(cb, "previous")
	previous := cb.MemoryLineageCopy()
	syncer := &guestMetricsEpochSyncerFake{err: errors.New("bolt write failed")}

	err := persistRuntimeSnapshotBinding(
		context.Background(), syncer, cb, "next",
	)
	require.ErrorContains(t, err, "persist runtime snapshot binding")
	require.Equal(t, []string{"sandbox"}, syncer.syncIDs)
	require.Equal(t, previous, cb.MemoryLineageCopy())
}

// A destination collision after the dirty bitmap is cleared must not reuse
// the sealed memory of an older package with the same ID.
func TestFailedCommitDoesNotResolveOldDestinationBaseline(t *testing.T) {
	cb := &cubeboxstore.CubeBox{Metadata: cubeboxstore.Metadata{ID: "sandbox"}}
	setRuntimeSnapshotBinding(cb, "T")
	syncer := &guestMetricsEpochSyncerFake{}
	require.NoError(t, persistRuntimeSnapshotBinding(context.Background(), syncer, cb, ""))
	oldBase, oldRestore := resolveBaseMemoryObjectFn, resolveRestoreBaseMemoryObjectFn
	oldCreate := createMemoryVolumeFor
	oldImported := getImportedSandboxMemoryFor
	defer func() {
		resolveBaseMemoryObjectFn, resolveRestoreBaseMemoryObjectFn = oldBase, oldRestore
		createMemoryVolumeFor = oldCreate
		getImportedSandboxMemoryFor = oldImported
	}()
	resolveBaseMemoryObjectFn = func(ctx context.Context, cb *cubeboxstore.CubeBox, backend string) (*storage.CowSnapshotObject, error) {
		// T is still present, but an invalid baseline must never look it up.
		if resolveBaseSnapshotID(cb) == "T" {
			t.Fatal("reused stale sealed memory baseline")
		}
		return resolveMemoryObjectFromSnapshotID(ctx, backend, resolveBaseSnapshotID(cb))
	}
	resolveRestoreBaseMemoryObjectFn = func(context.Context, *cubeboxstore.CubeBox, string) (*storage.CowSnapshotObject, error) {
		return nil, ErrNoBaseMemoryForIncremental
	}
	getImportedSandboxMemoryFor = func(context.Context, string, string) (*storage.CowSnapshotObject, error) { return nil, nil }
	snapshotErr, rootfsErr, _ := runSnapshotWithRootfs(
		func() error { return nil }, // successful memory capture clears bitmap
		func() error { return storage.ErrCowObjectAlreadyExists },
		func() error { return nil },
	)
	require.NoError(t, snapshotErr)
	require.ErrorIs(t, rootfsErr, storage.ErrCowObjectAlreadyExists)
	createMemoryVolumeFor = func(context.Context, string, string, uint64) (*storage.CowSnapshotObject, error) {
		return &storage.CowSnapshotObject{Name: "new-memory"}, nil
	}
	_, snapshotType, err := prepareCommitMemoryArtifact(context.Background(), log.G(context.Background()), cb, "new", 4096, "s3")
	require.NoError(t, err)
	require.Equal(t, snapshotTypeFull, snapshotType)
	// Failure to publish a completed package must leave the durable invalid
	// baseline in memory too, so the next commit follows the same safe fallback.
	syncer.err = errors.New("baseline publication unavailable")
	require.Error(t, persistRuntimeSnapshotBinding(context.Background(), syncer, cb, "new"))
	require.Equal(t, "", resolveBaseSnapshotID(cb))
	_, snapshotType, err = prepareCommitMemoryArtifact(context.Background(), log.G(context.Background()), cb, "next", 4096, "s3")
	require.NoError(t, err)
	require.Equal(t, snapshotTypeFull, snapshotType)
	syncer.err = nil
	require.NoError(t, persistRuntimeSnapshotBinding(context.Background(), syncer, cb, "new"))
	require.Equal(t, "new", resolveBaseSnapshotID(cb))
}
