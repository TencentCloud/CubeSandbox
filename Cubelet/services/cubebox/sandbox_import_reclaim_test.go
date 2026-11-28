// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

func stubImportFinish(t *testing.T) *[]bool {
	t.Helper()
	var finished []bool
	prev := finishImportClaimFn
	finishImportClaimFn = func(_ *storage.SandboxImportClaim, _ context.Context, owned bool) error {
		finished = append(finished, owned)
		return nil
	}
	t.Cleanup(func() { finishImportClaimFn = prev })
	return &finished
}

func stubStorageRecords(t *testing.T, records map[string]*storage.StorageInfo, err error) {
	t.Helper()
	prev := inspectStorageRecordsFn
	inspectStorageRecordsFn = func() (map[string]*storage.StorageInfo, error) { return records, err }
	t.Cleanup(func() { inspectStorageRecordsFn = prev })
}

func createResponse(code errorcode.ErrorCode) *cubebox.RunCubeSandboxResponse {
	return &cubebox.RunCubeSandboxResponse{Ret: &errorcode.Ret{RetCode: code}}
}

func TestFinishSandboxImportHandsDisksToTheirOwner(t *testing.T) {
	claim := &storage.SandboxImportClaim{SandboxID: "sb-1", Backend: "s3"}
	s := &service{}

	t.Run("successful create owns them", func(t *testing.T) {
		finished := stubImportFinish(t)
		stubStorageRecords(t, nil, errors.New("must not be asked"))
		s.finishSandboxImport(context.Background(), claim, createResponse(errorcode.ErrorCode_Success))
		require.Equal(t, []bool{true}, *finished)
	})
	t.Run("rejected create with no record releases them", func(t *testing.T) {
		finished := stubImportFinish(t)
		stubStorageRecords(t, map[string]*storage.StorageInfo{}, nil)
		s.finishSandboxImport(context.Background(), claim, createResponse(errorcode.ErrorCode_InvalidParamFormat))
		require.Equal(t, []bool{false}, *finished)
	})
	t.Run("failed create that left a storage record defers to destroy", func(t *testing.T) {
		finished := stubImportFinish(t)
		stubStorageRecords(t, map[string]*storage.StorageInfo{"sb-1": {SandboxID: "sb-1"}}, nil)
		s.finishSandboxImport(context.Background(), claim, createResponse(errorcode.ErrorCode_CreateStorageFailed))
		require.Equal(t, []bool{true}, *finished)
	})
	t.Run("unknown ownership keeps the claim", func(t *testing.T) {
		finished := stubImportFinish(t)
		stubStorageRecords(t, nil, errors.New("db unreadable"))
		s.finishSandboxImport(context.Background(), claim, createResponse(errorcode.ErrorCode_CreateStorageFailed))
		require.Empty(t, *finished)
	})
}
