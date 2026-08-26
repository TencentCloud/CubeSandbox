// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/pathutil"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

// ListLocalSnapshots returns catalog entries for the requested backend.
// Empty backend lists XFS then S3 as two separate namespaces (never mixed
// by reading catalog.json).
func (s *service) ListLocalSnapshots(ctx context.Context, req *cubebox.ListLocalSnapshotsRequest) (*cubebox.ListLocalSnapshotsResponse, error) {
	rsp := &cubebox.ListLocalSnapshotsResponse{
		RequestID: req.GetRequestID(),
		Ret:       &errorcode.Ret{RetCode: errorcode.ErrorCode_Success},
	}
	filter := strings.TrimSpace(req.GetBackend())
	backends := []string{cow.BackendXFS, cow.BackendS3}
	if filter != "" {
		normalized, err := resolveRequestStorageBackend(filter)
		if err != nil {
			rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
			rsp.Ret.RetMsg = err.Error()
			return rsp, nil
		}
		backends = []string{normalized}
	}
	rsp.Snapshots = make([]*cubebox.LocalSnapshotInfo, 0)
	for _, backend := range backends {
		entries, err := storage.ListLocalSnapshotsFor(ctx, backend)
		if err != nil {
			rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
			rsp.Ret.RetMsg = fmt.Sprintf("list local snapshots failed: %v", err)
			return rsp, nil
		}
		for _, e := range entries {
			info := localSnapshotEntryToProto(e)
			info.Backend = backend
			rsp.Snapshots = append(rsp.Snapshots, info)
		}
	}
	return rsp, nil
}

// GetLocalSnapshot returns the catalog entry for a single snapshot id in the
// requested backend namespace (empty backend = xfs).
func (s *service) GetLocalSnapshot(ctx context.Context, req *cubebox.GetLocalSnapshotRequest) (*cubebox.GetLocalSnapshotResponse, error) {
	rsp := &cubebox.GetLocalSnapshotResponse{
		RequestID: req.GetRequestID(),
		Ret:       &errorcode.Ret{RetCode: errorcode.ErrorCode_Success},
	}
	id := strings.TrimSpace(req.GetSnapshotID())
	if id == "" {
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = "snapshotID is required"
		return rsp, nil
	}
	if err := pathutil.ValidateSafeID(id); err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = fmt.Sprintf("invalid snapshotID: %v", err)
		return rsp, nil
	}
	backend, err := resolveRequestStorageBackend(req.GetBackend())
	if err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = err.Error()
		return rsp, nil
	}
	entry, err := storage.GetLocalSnapshotFor(ctx, backend, id)
	if err != nil {
		if errors.Is(err, storage.ErrSnapshotCatalogNotFound) {
			rsp.Ret.RetCode = errorcode.ErrorCode_PreConditionFailed
			rsp.Ret.RetMsg = fmt.Sprintf("snapshot %s not found on this node", id)
			return rsp, nil
		}
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("get local snapshot failed: %v", err)
		return rsp, nil
	}
	info := localSnapshotEntryToProto(entry)
	info.Backend = backend
	rsp.Snapshot = info
	return rsp, nil
}

func localSnapshotEntryToProto(e *storage.SnapshotCatalogEntry) *cubebox.LocalSnapshotInfo {
	if e == nil {
		return nil
	}
	return &cubebox.LocalSnapshotInfo{
		SnapshotID:      e.SnapshotID,
		InstanceType:    e.InstanceType,
		SpecDir:         e.SpecDir,
		SnapshotPath:    e.SnapshotPath,
		MetaDir:         e.MetaDir,
		RootfsVol:       e.RootfsVol,
		RootfsKind:      e.RootfsKind,
		MemoryVol:       e.MemoryVol,
		MemoryKind:      e.MemoryKind,
		RootfsSizeBytes: e.RootfsSizeBytes,
		CreatedAt:       e.CreatedAt,
		BuildRootfsVol:  e.BuildRootfsVol,
		BuildRootfsKind: e.BuildRootfsKind,
		Kind:            e.Kind,
	}
}
