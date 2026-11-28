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
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func crossNodeRestoreRequest(backend string) *cubebox.RunCubeSandboxRequest {
	return &cubebox.RunCubeSandboxRequest{
		RequestID: "req-1",
		Annotations: map[string]string{
			constants.MasterAnnotationStorageBackend:      backend,
			constants.MasterAnnotationDesiredSandboxID:    "sb-1",
			constants.MasterAnnotationSnapshotRemoteUUIDs: `{"rootfs":"r1","memory":"m1","metadata":"d1"}`,
			constants.MasterAnnotationSnapshotCrossNode:   "true",
		},
	}
}

type storeReadyCall struct {
	backend string
	limit   time.Duration
}

func stubCrossNodeStoreReady(t *testing.T, result error) *[]storeReadyCall {
	t.Helper()
	calls := &[]storeReadyCall{}
	prev := waitCrossNodeStoreReadyFn
	waitCrossNodeStoreReadyFn = func(_ context.Context, backend string, limit time.Duration) error {
		*calls = append(*calls, storeReadyCall{backend: backend, limit: limit})
		return result
	}
	t.Cleanup(func() { waitCrossNodeStoreReadyFn = prev })
	return calls
}

// A node that was just added (for example by a cluster autoscaler) is
// schedulable before its S3 handle is published. The
// restore must wait for the backend before importing anything, and a node
// that stays uninitialized must fail with the backend's own not-ready error,
// not a metadata import error.
func TestPrepareCrossNodeRestoreWaitsForBackendBeforeImport(t *testing.T) {
	calls := stubCrossNodeStoreReady(t, storage.ErrS3NotReady)

	_, _, err := prepareCrossNodeRestore(context.Background(), crossNodeRestoreRequest("s3"))
	require.ErrorIs(t, err, storage.ErrS3NotReady)
	require.NotContains(t, err.Error(), "import metadata")
	require.Equal(t, []storeReadyCall{{backend: "s3", limit: crossNodeStoreReadyWait}}, *calls)
}

func stubClaimSandboxImport(t *testing.T) *[]string {
	t.Helper()
	claimed := &[]string{}
	prev := claimSandboxImportFn
	claimSandboxImportFn = func(id, backend string) (*storage.SandboxImportClaim, error) {
		*claimed = append(*claimed, backend+":"+id)
		return &storage.SandboxImportClaim{SandboxID: id, Backend: backend}, nil
	}
	t.Cleanup(func() { claimSandboxImportFn = prev })
	return claimed
}

func TestPrepareCrossNodeRestoreProceedsToImportOnceBackendReady(t *testing.T) {
	calls := stubCrossNodeStoreReady(t, nil)
	claimed := stubClaimSandboxImport(t)

	_, _, err := prepareCrossNodeRestore(context.Background(), crossNodeRestoreRequest("s3"))
	require.Len(t, *calls, 1)
	require.Len(t, *claimed, 1)
	// No storage is initialized in this unit test, so the import itself
	// fails; reaching it is what proves the wait let the restore through.
	require.Error(t, err)
	require.Contains(t, err.Error(), "import metadata for cross-node restore of sb-1")
}

// The metadata disk is imported before any sandbox record can own it, so the
// import is claimed first, and a failed import still hands the claim back
// for the Create to settle.
func TestPrepareCrossNodeRestoreClaimsBeforeImport(t *testing.T) {
	stubCrossNodeStoreReady(t, nil)
	claimed := stubClaimSandboxImport(t)

	_, claim, err := prepareCrossNodeRestore(context.Background(), crossNodeRestoreRequest("s3"))
	require.Error(t, err)
	require.Equal(t, []string{"s3:sb-1"}, *claimed)
	require.NotNil(t, claim)
	require.Equal(t, "sb-1", claim.SandboxID)
}

// Without a claim nothing is imported.
func TestPrepareCrossNodeRestoreRefusesUnclaimedImport(t *testing.T) {
	stubCrossNodeStoreReady(t, nil)
	prev := claimSandboxImportFn
	claimSandboxImportFn = func(string, string) (*storage.SandboxImportClaim, error) { return nil, errors.New("db down") }
	t.Cleanup(func() { claimSandboxImportFn = prev })

	_, claim, err := prepareCrossNodeRestore(context.Background(), crossNodeRestoreRequest("s3"))
	require.ErrorContains(t, err, "db down")
	require.NotContains(t, err.Error(), "import metadata")
	require.Nil(t, claim)
}

func TestPrepareCrossNodeRestoreSkipsWaitForSameNodeRestore(t *testing.T) {
	calls := stubCrossNodeStoreReady(t, errors.New("must not be called"))
	req := crossNodeRestoreRequest("s3")
	delete(req.Annotations, constants.MasterAnnotationSnapshotCrossNode)

	_, _, err := prepareCrossNodeRestore(context.Background(), req)
	require.NoError(t, err)
	require.Empty(t, *calls)
}
