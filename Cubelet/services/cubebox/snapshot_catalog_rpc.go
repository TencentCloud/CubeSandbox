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
)

// ListLocalSnapshots returns every snapshot catalog entry this cubelet knows
// about. It is intended to let master rebuild its view of which snapshots
// physically exist on the node without having to persist vol/dev/path on its
// own side.
func (s *service) ListLocalSnapshots(ctx context.Context, req *cubebox.ListLocalSnapshotsRequest) (*cubebox.ListLocalSnapshotsResponse, error) {
	rsp := &cubebox.ListLocalSnapshotsResponse{
		RequestID: req.GetRequestID(),
		Ret:       &errorcode.Ret{RetCode: errorcode.ErrorCode_Success},
	}
	entries, err := storage.ListLocalSnapshots(ctx)
	if err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("list local snapshots failed: %v", err)
		return rsp, nil
	}
	rsp.Snapshots = make([]*cubebox.LocalSnapshotInfo, 0, len(entries))
	for _, e := range entries {
		rsp.Snapshots = append(rsp.Snapshots, localSnapshotEntryToProto(e))
	}
	return rsp, nil
}

// GetLocalSnapshot returns the catalog entry for a single snapshot id. Missing
// records produce PreConditionFailed with a clear message; callers should
// treat that as authoritative "not on this node".
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
	entry, err := storage.GetLocalSnapshot(ctx, id)
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
	rsp.Snapshot = localSnapshotEntryToProto(entry)
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
