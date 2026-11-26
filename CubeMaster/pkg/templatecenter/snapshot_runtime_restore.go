// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxspec"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gorm.io/gorm"
)

// CheckSnapshotRuntimeVolumeRestore is called with the trusted, frozen request
// before using a memory image. The current runtime has no hot-plug device mapping.
func CheckSnapshotRuntimeVolumeRestore(req *sandboxtypes.CreateCubeSandboxReq) error {
	if req != nil {
		if _, captured := req.Annotations[constants.CubeAnnotationSnapshotRuntimeVolumeMounts]; captured {
			return fmt.Errorf("%w: memory restore of runtime-attached volumes requires device reattachment", ErrTemplateAttemptInProgress)
		}
	}
	return nil
}

func checkRuntimeVolumeRollback(ctx context.Context, sandboxID string, target *models.SnapshotRecord) error {
	if err := sandboxspec.EnsureNoPendingRuntimeAttach(ctx, sandboxID); err != nil {
		if errors.Is(err, sandboxspec.ErrRuntimeAttachPending) {
			return fmt.Errorf("%w: %w", ErrTemplateAttemptInProgress, err)
		}
		return err
	}
	current, err := sandboxspec.GetWithRuntimeVolumeMounts(ctx, sandboxID)
	if err != nil && !errors.Is(err, sandboxspec.ErrSandboxSpecNotFound) {
		return err
	}
	if current != nil && len(current.Mounts) != 0 {
		return fmt.Errorf("%w: rollback of a sandbox with runtime-attached volumes requires device reattachment", ErrTemplateAttemptInProgress)
	}
	frozen, err := requestFromSnapshotJSON(target.RequestJSON)
	if err != nil {
		return fmt.Errorf("read snapshot mount configuration: %w", err)
	}
	return CheckSnapshotRuntimeVolumeRestore(frozen)
}

func lockSnapshotCaptureMounts(tx *gorm.DB, sandboxID string, frozen *sandboxtypes.CreateCubeSandboxReq) error {
	var expected []sandboxspec.RuntimeVolumeMount
	if frozen != nil {
		if raw, ok := frozen.Annotations[constants.CubeAnnotationSnapshotRuntimeVolumeMounts]; ok {
			if err := json.Unmarshal([]byte(raw), &expected); err != nil {
				return fmt.Errorf("read frozen runtime mounts: %w", err)
			}
		}
	}
	err := sandboxspec.LockRuntimeMountsForCaptureInTx(tx, sandboxID, expected)
	if errors.Is(err, sandboxspec.ErrRuntimeAttachPending) || errors.Is(err, sandboxspec.ErrRuntimeMountsChanged) {
		return fmt.Errorf("%w: %w", ErrTemplateAttemptInProgress, err)
	}
	return err
}
