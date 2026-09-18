// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
)

func TestSetRuntimeSnapshotBindingLabels(t *testing.T) {
	cb := &cubeboxstore.CubeBox{}
	attachedAt := time.Date(2026, 5, 10, 9, 0, 0, 0, time.UTC)

	setRuntimeSnapshotBindingLabels(cb, "snap-1", attachedAt)

	if got := cb.Labels[constants.MasterAnnotationRuntimeSnapshotID]; got != "snap-1" {
		t.Fatalf("runtime snapshot id = %q, want snap-1", got)
	}
	if got := cb.Labels[constants.MasterAnnotationRuntimeSnapshotAttachedAt]; got != attachedAt.Format(time.RFC3339Nano) {
		t.Fatalf("runtime snapshot attached at = %q, want %q", got, attachedAt.Format(time.RFC3339Nano))
	}
}

func TestSetRuntimeSnapshotBindingLabelsSkipsEmptyID(t *testing.T) {
	cb := &cubeboxstore.CubeBox{}
	setRuntimeSnapshotBindingLabels(cb, "", time.Now().UTC())
	if len(cb.Labels) != 0 {
		t.Fatalf("expected no labels when snapshot id empty, got %v", cb.Labels)
	}
}

func TestPersistRuntimeSnapshotBindingUpdatesDurableBaseline(t *testing.T) {
	attachedAt := time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)
	cb := &cubeboxstore.CubeBox{Metadata: cubeboxstore.Metadata{
		ID:     "sandbox",
		Labels: map[string]string{constants.MasterAnnotationRuntimeSnapshotID: "previous"},
	}}
	syncer := &guestMetricsEpochSyncerFake{}

	require.NoError(t, persistRuntimeSnapshotBinding(
		context.Background(), syncer, cb, "next", attachedAt,
	))
	require.Equal(t, []string{"sandbox"}, syncer.syncIDs)
	require.Equal(t, "next", cb.Labels[constants.MasterAnnotationRuntimeSnapshotID])
	require.Equal(t, attachedAt.Format(time.RFC3339Nano), cb.Labels[constants.MasterAnnotationRuntimeSnapshotAttachedAt])
}

func TestPersistRuntimeSnapshotBindingRestoresLabelsOnFailure(t *testing.T) {
	previousLabels := map[string]string{
		constants.MasterAnnotationRuntimeSnapshotID:         "previous",
		constants.MasterAnnotationRuntimeSnapshotAttachedAt: "previous-time",
		"unrelated": "preserved",
	}
	cb := &cubeboxstore.CubeBox{Metadata: cubeboxstore.Metadata{
		ID:     "sandbox",
		Labels: previousLabels,
	}}
	syncer := &guestMetricsEpochSyncerFake{err: errors.New("bolt write failed")}

	err := persistRuntimeSnapshotBinding(
		context.Background(), syncer, cb, "next", time.Now().UTC(),
	)
	require.ErrorContains(t, err, "persist runtime snapshot binding")
	require.Equal(t, []string{"sandbox"}, syncer.syncIDs)
	require.Equal(t, previousLabels, cb.Labels)
}

// A destination collision after the dirty bitmap is cleared must not reuse
// the sealed memory of an older package with the same ID.
func TestFailedCommitDoesNotResolveOldDestinationBaseline(t *testing.T) {
	cb := &cubeboxstore.CubeBox{Metadata: cubeboxstore.Metadata{ID: "sandbox"}}
	setRuntimeSnapshotBindingLabels(cb, "T", time.Now())
	syncer := &guestMetricsEpochSyncerFake{}
	require.NoError(t, persistRuntimeSnapshotBinding(context.Background(), syncer, cb, runtimeSnapshotBindingInvalidID, time.Now()))
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
	snapshotErr, rootfsErr, _ := runSnapshotWithRootfs(true,
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
	require.Error(t, persistRuntimeSnapshotBinding(context.Background(), syncer, cb, "new", time.Now()))
	require.Equal(t, runtimeSnapshotBindingInvalidID, resolveBaseSnapshotID(cb))
	_, snapshotType, err = prepareCommitMemoryArtifact(context.Background(), log.G(context.Background()), cb, "next", 4096, "s3")
	require.NoError(t, err)
	require.Equal(t, snapshotTypeFull, snapshotType)
	syncer.err = nil
	require.NoError(t, persistRuntimeSnapshotBinding(context.Background(), syncer, cb, "new", time.Now()))
	require.Equal(t, "new", resolveBaseSnapshotID(cb))
}
