// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

func TestImportSnapshotSuccessResponse(t *testing.T) {
	origImportSnapshotFn := importSnapshotFn
	origGetSnapshotInfoFn := getSnapshotInfoFn
	t.Cleanup(func() {
		importSnapshotFn = origImportSnapshotFn
		getSnapshotInfoFn = origGetSnapshotInfoFn
	})

	importSnapshotFn = func(ctx context.Context, requestID, hostID, hostIP, rootfsSource string, createSpec *types.CreateCubeSandboxReq) (*types.TemplateImageJobInfo, error) {
		assert.Equal(t, "req-1", requestID)
		assert.Equal(t, "node-a", hostID)
		assert.Equal(t, "10.0.0.1", hostIP)
		assert.Equal(t, "/data/cubelet/storage/import/rootfs.vol", rootfsSource)
		if assert.NotNil(t, createSpec) && assert.Len(t, createSpec.Containers, 1) {
			assert.Equal(t, "busybox:latest", createSpec.Containers[0].Image.Image)
		}
		return &types.TemplateImageJobInfo{
			JobID:        "op-1",
			TemplateID:   "snap-1",
			RequestID:    requestID,
			ResourceType: "snapshot",
			ResourceID:   "snap-1",
			Operation:    templatecenter.JobOperationSnapshotImport,
			Status:       "READY",
			Phase:        "REGISTERING",
		}, nil
	}
	getSnapshotInfoFn = func(ctx context.Context, snapshotID string, includeRequest bool) (*templatecenter.SnapshotInfo, error) {
		return &templatecenter.SnapshotInfo{
			SnapshotID:     snapshotID,
			Status:         "READY",
			StorageBackend: "cubecow",
		}, nil
	}

	req := httptest.NewRequest(http.MethodPost, "/cube/snapshot/import", strings.NewReader(`{
		"request_id":"req-1",
		"host_id":"node-a",
		"host_ip":"10.0.0.1",
		"rootfs_source_path":"/data/cubelet/storage/import/rootfs.vol",
		"create_spec":{"containers":[{"name":"c0","image":{"image":"busybox:latest"}}]}
	}`))
	rt := &CubeLog.RequestTrace{}
	resp := importSnapshot(req, rt)

	got, ok := resp.(*snapshotResponse)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, int(errorcode.ErrorCode_Success), got.Ret.RetCode)
	assert.Equal(t, "req-1", got.Res.RequestID)
	if assert.NotNil(t, got.Snapshot) {
		assert.Equal(t, "snap-1", got.Snapshot.SnapshotID)
		assert.Equal(t, "READY", got.Snapshot.Status)
	}
	if assert.NotNil(t, got.Operation) {
		assert.Equal(t, "op-1", got.Operation.OperationID)
		assert.Equal(t, "snap-1", got.Operation.SnapshotID)
		assert.Equal(t, "READY", got.Operation.Status)
	}
	assert.Equal(t, int64(errorcode.ErrorCode_Success), rt.RetCode)
}

func TestImportSnapshotRejectsMissingFields(t *testing.T) {
	origImportSnapshotFn := importSnapshotFn
	t.Cleanup(func() { importSnapshotFn = origImportSnapshotFn })
	importSnapshotFn = func(ctx context.Context, requestID, hostID, hostIP, rootfsSource string, createSpec *types.CreateCubeSandboxReq) (*types.TemplateImageJobInfo, error) {
		t.Fatal("importSnapshotFn must not be called for an invalid request")
		return nil, nil
	}

	base := map[string]string{
		"request_id":         `"req-1"`,
		"host_ip":            `"10.0.0.1"`,
		"rootfs_source_path": `"/data/cubelet/storage/import/rootfs.vol"`,
		"create_spec":        `{"containers":[]}`,
	}
	for _, missing := range []string{"request_id", "host_ip", "rootfs_source_path", "create_spec"} {
		var fields []string
		for k, v := range base {
			if k != missing {
				fields = append(fields, `"`+k+`":`+v)
			}
		}
		req := httptest.NewRequest(http.MethodPost, "/cube/snapshot/import",
			strings.NewReader("{"+strings.Join(fields, ",")+"}"))
		resp := importSnapshot(req, &CubeLog.RequestTrace{})
		got, ok := resp.(*snapshotResponse)
		if !ok {
			t.Fatalf("missing %s: unexpected response type %T", missing, resp)
		}
		assert.Equalf(t, int(errorcode.ErrorCode_MasterParamsError), got.Ret.RetCode, "missing %s", missing)
		assert.Containsf(t, got.Ret.RetMsg, missing, "missing %s", missing)
	}
}

func TestImportSnapshotRejectsNonCubeboxInstanceType(t *testing.T) {
	origImportSnapshotFn := importSnapshotFn
	t.Cleanup(func() { importSnapshotFn = origImportSnapshotFn })
	importSnapshotFn = func(ctx context.Context, requestID, hostID, hostIP, rootfsSource string, createSpec *types.CreateCubeSandboxReq) (*types.TemplateImageJobInfo, error) {
		t.Fatal("importSnapshotFn must not be called for an invalid request")
		return nil, nil
	}

	req := httptest.NewRequest(http.MethodPost, "/cube/snapshot/import", strings.NewReader(`{
		"request_id":"req-1",
		"host_ip":"10.0.0.1",
		"rootfs_source_path":"/data/cubelet/storage/import/rootfs.vol",
		"create_spec":{"instance_type":"cube-v2","containers":[{"name":"c0","image":{"image":"busybox:latest"}}]}
	}`))
	resp := importSnapshot(req, &CubeLog.RequestTrace{})
	got, ok := resp.(*snapshotResponse)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), got.Ret.RetCode)
	assert.Contains(t, got.Ret.RetMsg, "does not support snapshot import")
}
