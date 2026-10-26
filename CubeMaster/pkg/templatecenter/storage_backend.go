// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func storageBackendFromCreate(req *sandboxtypes.CreateCubeSandboxReq) string {
	if req == nil {
		return ""
	}
	if b := strings.TrimSpace(req.Backend); b != "" {
		return b
	}
	if req.Annotations == nil {
		return ""
	}
	return strings.TrimSpace(req.Annotations[constants.CubeAnnotationStorageBackend])
}

// stampCreateRequestBackend writes a normalized xfs|s3 backend onto the
// create request and the Master→Cubelet annotation. Empty input is a no-op
// so callers that omit backend keep the historical request shape.
func stampCreateRequestBackend(req *sandboxtypes.CreateCubeSandboxReq, backend string) error {
	if req == nil || strings.TrimSpace(backend) == "" {
		return nil
	}
	normalized, err := constants.NormalizeSnapshotBackend(backend)
	if err != nil {
		return err
	}
	req.Backend = normalized
	if req.Annotations == nil {
		req.Annotations = map[string]string{}
	}
	req.Annotations[constants.CubeAnnotationStorageBackend] = normalized
	return nil
}

// clearCreateRequestBackend drops a client-supplied backend so later stages
// cannot override the value Master persisted at template create.
func clearCreateRequestBackend(req *sandboxtypes.CreateCubeSandboxReq) {
	if req == nil {
		return
	}
	req.Backend = ""
	if req.Annotations != nil {
		delete(req.Annotations, constants.CubeAnnotationStorageBackend)
	}
}

// applyStoredCreateBackend writes the DB column onto req. The column is the
// source of truth; request JSON / client fields are ignored when the row is
// pinned. Missing or historical values leave the request unchanged.
func applyStoredCreateBackend(req *sandboxtypes.CreateCubeSandboxReq, stored string) error {
	if req == nil {
		return nil
	}
	if !isPinnedSnapshotBackend(stored) {
		return nil
	}
	if err := stampCreateRequestBackend(req, stored); err != nil {
		return nil
	}
	return nil
}

// resolvePersistedCreateBackend returns the xfs|s3 backend Master already
// stored on a sandbox/template create request. Empty input becomes xfs.
func resolvePersistedCreateBackend(req *sandboxtypes.CreateCubeSandboxReq) (string, error) {
	return constants.NormalizeSnapshotBackend(storageBackendFromCreate(req))
}

func isPinnedSnapshotBackend(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case constants.SnapshotBackendXFS, constants.SnapshotBackendS3:
		return true
	default:
		return false
	}
}

// pinnedCleanupBackend returns xfs|s3 only when the row was explicitly
// pinned. Empty / historical cubecow stay empty so Cleanup keeps the
// historical Cubelet default (xfs).
func pinnedCleanupBackend(raw string) string {
	if !isPinnedSnapshotBackend(raw) {
		return ""
	}
	normalized, err := constants.NormalizeSnapshotBackend(raw)
	if err != nil {
		return ""
	}
	return normalized
}

func cleanupBackendFromTargets(targets *templateCleanupTargets) string {
	if targets == nil {
		return ""
	}
	if targets.Snapshot != nil {
		if hint := pinnedCleanupBackend(targets.Snapshot.Backend); hint != "" {
			return hint
		}
	}
	if targets.Definition != nil {
		return pinnedCleanupBackend(targets.Definition.StorageBackend)
	}
	return ""
}

// InheritCreateBackendFromTemplate stamps the template's persisted backend
// onto the create request. Clients may only choose backend when creating a
// template; later stages always follow CubeMaster DB. A request-supplied
// backend is discarded. Both empty leaves the request unchanged.
func InheritCreateBackendFromTemplate(req, templateReq *sandboxtypes.CreateCubeSandboxReq) error {
	if req == nil {
		return nil
	}
	clearCreateRequestBackend(req)
	return stampCreateRequestBackend(req, storageBackendFromCreate(templateReq))
}
