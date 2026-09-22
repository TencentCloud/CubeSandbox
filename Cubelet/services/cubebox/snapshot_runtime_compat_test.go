// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"errors"
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"

	"github.com/stretchr/testify/require"
)

func TestRunSnapshotWithRootfs(t *testing.T) {
	failure := errors.New("injected failure")
	for _, tc := range []struct {
		name                              string
		snapshotErr, rootfsErr, resumeErr error
		wantCalls                         []string
	}{
		{"success", nil, nil, nil, []string{"memory", "rootfs", "resume"}},
		{"memory failure", failure, nil, nil, []string{"memory", "resume"}},
		{"rootfs failure", nil, failure, nil, []string{"memory", "rootfs", "resume"}},
		{"resume failure", nil, nil, failure, []string{"memory", "rootfs", "resume"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			snapshotErr, rootfsErr, resumeErr := runSnapshotWithRootfs(
				func() error { calls = append(calls, "memory"); return tc.snapshotErr },
				func() error { calls = append(calls, "rootfs"); return tc.rootfsErr },
				func() error { calls = append(calls, "resume"); return tc.resumeErr },
			)
			require.Equal(t, tc.wantCalls, calls)
			require.Equal(t, tc.snapshotErr, snapshotErr)
			require.Equal(t, tc.rootfsErr, rootfsErr)
			require.Equal(t, tc.resumeErr, resumeErr)
		})
	}
}

func TestCommitSnapshotDestinationRejectsExistingPackage(t *testing.T) {
	for _, name := range []string{"tpl-T-rootfs", "tpl-T-memory", "tpl-T-memory-snap", storage.S3MetadataSnapshotName("T")} {
		t.Run(name, func(t *testing.T) {
			err := checkCommitSnapshotDestination(context.Background(), "s3", "T",
				func(_ context.Context, backend string, refs []storage.CowObjectRef) ([]storage.CowObjectStatus, error) {
					require.Equal(t, "s3", backend)
					for _, ref := range refs {
						if ref.Name == name {
							return []storage.CowObjectStatus{{Name: name, Kind: ref.Kind, Exists: true}}, nil
						}
					}
					t.Fatalf("existing object %s was not checked", name)
					return nil, nil
				})
			require.ErrorIs(t, err, storage.ErrCowObjectAlreadyExists)
		})
	}
	dbErr := errors.New("inspect unavailable")
	err := checkCommitSnapshotDestination(context.Background(), "s3", "T",
		func(context.Context, string, []storage.CowObjectRef) ([]storage.CowObjectStatus, error) {
			return nil, dbErr
		})
	require.ErrorIs(t, err, dbErr)
	require.NoError(t, checkCommitSnapshotDestination(context.Background(), "xfs", "new",
		func(context.Context, string, []storage.CowObjectRef) ([]storage.CowObjectStatus, error) {
			return nil, nil
		}))
}
