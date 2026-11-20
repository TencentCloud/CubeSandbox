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
	for _, backend := range []string{"s3"} {
		calls := stubCrossNodeStoreReady(t, storage.ErrS3NotReady)

		_, err := prepareCrossNodeRestore(context.Background(), crossNodeRestoreRequest(backend))
		require.ErrorIs(t, err, storage.ErrS3NotReady)
		require.NotContains(t, err.Error(), "import metadata")
		require.Equal(t, []storeReadyCall{{backend: backend, limit: crossNodeStoreReadyWait}}, *calls)
	}
}

func TestPrepareCrossNodeRestoreProceedsToImportOnceBackendReady(t *testing.T) {
	calls := stubCrossNodeStoreReady(t, nil)

	_, err := prepareCrossNodeRestore(context.Background(), crossNodeRestoreRequest("s3"))
	require.Len(t, *calls, 1)
	// No storage is initialized in this unit test, so the import itself
	// fails; reaching it is what proves the wait let the restore through.
	require.Error(t, err)
	require.Contains(t, err.Error(), "import metadata for cross-node restore of sb-1")
}

func TestPrepareCrossNodeRestoreSkipsWaitForSameNodeRestore(t *testing.T) {
	calls := stubCrossNodeStoreReady(t, errors.New("must not be called"))
	req := crossNodeRestoreRequest("s3")
	delete(req.Annotations, constants.MasterAnnotationSnapshotCrossNode)

	_, err := prepareCrossNodeRestore(context.Background(), req)
	require.NoError(t, err)
	require.Empty(t, *calls)
}
