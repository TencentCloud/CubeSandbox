// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/pathutil"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
)

func (s *service) ListSandboxSnapshots(ctx context.Context, req *cubebox.ListSandboxSnapshotsRequest) (*cubebox.ListSandboxSnapshotsResponse, error) {
	rsp := &cubebox.ListSandboxSnapshotsResponse{
		RequestID: req.GetRequestID(),
		SandboxID: strings.TrimSpace(req.GetSandboxID()),
		Ret:       &errorcode.Ret{RetCode: errorcode.ErrorCode_Success},
	}
	if rsp.SandboxID != "" {
		if err := pathutil.ValidateSafeID(rsp.SandboxID); err != nil {
			rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
			rsp.Ret.RetMsg = fmt.Sprintf("invalid sandboxID: %v", err)
			return rsp, nil
		}
	}
	refs, err := cubecowObjectRefsForInspection(req.GetObjects())
	if err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = err.Error()
		return rsp, nil
	}
	if !storage.IsCowBackend() {
		rsp.Ret.RetCode = errorcode.ErrorCode_PreConditionFailed
		rsp.Ret.RetMsg = "ListSandboxSnapshots requires storage_backend=cubecow"
		return rsp, nil
	}
	statuses, err := storage.InspectCowObjects(ctx, refs)
	if err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("failed to inspect cubecow objects: %v", err)
		return rsp, nil
	}
	pathStatus, err := inspectSnapshotPaths(req.GetMetaDir())
	if err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = err.Error()
		return rsp, nil
	}
	rsp.Objects = make([]*cubebox.CowObjectStatus, 0, len(statuses))
	for _, status := range statuses {
		rsp.Objects = append(rsp.Objects, &cubebox.CowObjectStatus{
			Name:         status.Name,
			Kind:         status.Kind,
			Role:         status.Role,
			Exists:       status.Exists,
			DevicePath:   status.DevicePath,
			SizeBytes:    status.SizeBytes,
			ErrorMessage: status.ErrorMessage,
		})
	}
	rsp.MetaDirExists = pathStatus.metaDirExists
	rsp.SnapshotStatePath = pathStatus.snapshotStatePath
	rsp.SnapshotStateExists = pathStatus.snapshotStateExists
	rsp.PathErrorMessage = pathStatus.errorMessage
	return rsp, nil
}

func (s *service) GetStorageMetrics(ctx context.Context, req *cubebox.GetStorageMetricsRequest) (*cubebox.GetStorageMetricsResponse, error) {
	rsp := &cubebox.GetStorageMetricsResponse{
		RequestID: req.GetRequestID(),
		Ret:       &errorcode.Ret{RetCode: errorcode.ErrorCode_Success},
	}
	if !storage.IsCowBackend() {
		rsp.Ret.RetCode = errorcode.ErrorCode_PreConditionFailed
		rsp.Ret.RetMsg = "GetStorageMetrics requires storage_backend=cubecow"
		return rsp, nil
	}
	nodeID, err := utils.GetInstanceID()
	if err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("failed to resolve node id: %v", err)
		return rsp, nil
	}
	metrics, err := storage.GetCowMetrics(ctx)
	if err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("failed to collect storage metrics: %v", err)
		return rsp, nil
	}
	rsp.NodeId = nodeID
	rsp.TimestampUnixNano = time.Now().UnixNano()
	rsp.Metrics = metrics
	return rsp, nil
}

func cubecowObjectRefsForInspection(objects []*cubebox.CowObjectRef) ([]storage.CowObjectRef, error) {
	return parseCowObjectRefs(objects)
}

type snapshotPathInspection struct {
	metaDirExists       bool
	snapshotStatePath   string
	snapshotStateExists bool
	errorMessage        string
}

func inspectSnapshotPaths(metaDir string) (snapshotPathInspection, error) {
	metaDir = strings.TrimSpace(metaDir)
	if metaDir == "" {
		return snapshotPathInspection{}, nil
	}
	if err := pathutil.ValidateNoTraversal(metaDir); err != nil {
		return snapshotPathInspection{}, fmt.Errorf("invalid meta_dir: %w", err)
	}
	cleanMetaDir := filepath.Clean(metaDir)
	if !filepath.IsAbs(cleanMetaDir) {
		return snapshotPathInspection{}, fmt.Errorf("invalid meta_dir %q: must be absolute", metaDir)
	}
	status := snapshotPathInspection{
		snapshotStatePath: snapshotStateDir(cleanMetaDir),
	}
	metaInfo, metaErr := os.Stat(cleanMetaDir)
	if metaErr == nil && metaInfo.IsDir() {
		status.metaDirExists = true
	} else if metaErr == nil {
		status.errorMessage = fmt.Sprintf("meta_dir %s is not a directory", cleanMetaDir)
	} else if !os.IsNotExist(metaErr) {
		status.errorMessage = fmt.Sprintf("inspect meta_dir %s failed: %v", cleanMetaDir, metaErr)
	}
	stateInfo, stateErr := os.Stat(status.snapshotStatePath)
	if stateErr == nil && stateInfo.IsDir() {
		status.snapshotStateExists = true
	} else if stateErr == nil {
		status.errorMessage = firstPathError(status.errorMessage, fmt.Sprintf("snapshot state path %s is not a directory", status.snapshotStatePath))
	} else if !os.IsNotExist(stateErr) {
		status.errorMessage = firstPathError(status.errorMessage, fmt.Sprintf("inspect snapshot state path %s failed: %v", status.snapshotStatePath, stateErr))
	}
	return status, nil
}

func firstPathError(current, next string) string {
	if strings.TrimSpace(current) != "" {
		return current
	}
	return next
}
