// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandboxspec

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ErrRuntimeAttachPending means lifecycle operations cannot proceed until the
// admitted Attach has a known result.
var ErrRuntimeAttachPending = errors.New("sandbox runtime attach is pending")

var ErrRuntimeMountsChanged = errors.New("sandbox runtime mounts changed before capture")

// EnsureNoPendingRuntimeAttach checks the active spec while the caller holds
// the sandbox lifecycle lock. Legacy sandboxes may have no spec row.
func EnsureNoPendingRuntimeAttach(ctx context.Context, sandboxID string) error {
	client := getDB()
	if client == nil {
		return ErrSandboxSpecStoreNotReady
	}

	var record models.SandboxSpec
	err := client.WithContext(ctx).Select("pending_runtime_attach").
		Where("sandbox_id = ?", strings.TrimSpace(sandboxID)).Take(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if record.PendingRuntimeAttach != nil {
		return ErrRuntimeAttachPending
	}
	return nil
}

// ClaimRuntimeAttachInTx sets the marker on the exact active spec row in the
// same transaction that reserves the volume.
func ClaimRuntimeAttachInTx(tx *gorm.DB, sandboxID string, rowID uint, token string) error {
	result := tx.Model(&models.SandboxSpec{}).
		Where("sandbox_id = ? AND id = ? AND pending_runtime_attach IS NULL", sandboxID, rowID).
		UpdateColumn("pending_runtime_attach", token)
	if result.Error != nil || result.RowsAffected == 1 {
		return result.Error
	}

	var record models.SandboxSpec
	err := tx.Select("id").Where("sandbox_id = ? AND id = ?", sandboxID, rowID).Take(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrSandboxSpecNotFound
	}
	if err != nil {
		return err
	}
	return ErrRuntimeAttachPending
}

// ClearRuntimeAttachInTx clears only the marker owned by this admission.
func ClearRuntimeAttachInTx(tx *gorm.DB, sandboxID string, rowID uint, token string) error {
	var record models.SandboxSpec
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("sandbox_id", "pending_runtime_attach").Take(&record, rowID).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if err != nil || record.SandboxID != sandboxID || record.PendingRuntimeAttach == nil || *record.PendingRuntimeAttach != token {
		return errors.New("sandbox runtime attach marker changed before settlement")
	}
	result := tx.Model(&models.SandboxSpec{}).
		Where("sandbox_id = ? AND id = ? AND pending_runtime_attach = ?", sandboxID, rowID, token).
		UpdateColumn("pending_runtime_attach", nil)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errors.New("sandbox runtime attach marker changed before settlement")
	}
	return nil
}

// LockRuntimeMountsForCaptureInTx verifies that the frozen bindings still
// match the live spec. The row stays locked until the capture job is recorded.
func LockRuntimeMountsForCaptureInTx(tx *gorm.DB, sandboxID string, expected []RuntimeVolumeMount) error {
	record, err := lockRuntimeAttachIdleSpec(tx, sandboxID)
	if err != nil {
		return err
	}
	if record == nil {
		if len(expected) == 0 {
			return nil
		}
		return ErrRuntimeMountsChanged
	}
	mounts, err := decodeRuntimeVolumeMounts(record)
	if err != nil {
		return err
	}
	if !slices.Equal(mounts, expected) {
		return ErrRuntimeMountsChanged
	}
	return nil
}

// LockNoPendingRuntimeAttachInTx excludes an attach until the caller has
// recorded its own lifecycle work on an active spec.
func LockNoPendingRuntimeAttachInTx(tx *gorm.DB, sandboxID string) error {
	_, err := lockRuntimeAttachIdleSpec(tx, sandboxID)
	return err
}

func lockRuntimeAttachIdleSpec(tx *gorm.DB, sandboxID string) (*models.SandboxSpec, error) {
	var record models.SandboxSpec
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("pending_runtime_attach", "runtime_volume_mounts").
		Where("sandbox_id = ?", sandboxID).Take(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if record.PendingRuntimeAttach != nil {
		return nil, ErrRuntimeAttachPending
	}
	return &record, nil
}
