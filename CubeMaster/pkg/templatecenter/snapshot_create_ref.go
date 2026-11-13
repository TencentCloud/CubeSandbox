// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const SnapshotRuntimeBindingCreatePending = "create_pending"

// ReserveSnapshotCreate pins the source before issuing a create RPC. The caller
// gives Cubelet this exact sandbox ID. Unlike a process-local lock, the pin
// survives a lost response or a Master restart. Registration or authoritative
// node reconciliation turns it into the normal memory_backing binding.
func ReserveSnapshotCreate(ctx context.Context, snapshotID, sandboxID, nodeID, nodeIP string) error {
	if !isReady() {
		return ErrTemplateStoreNotInitialized
	}
	if strings.TrimSpace(snapshotID) == "" || strings.TrimSpace(sandboxID) == "" ||
		(strings.TrimSpace(nodeID) == "" && strings.TrimSpace(nodeIP) == "") {
		return errors.New("snapshot create reservation requires snapshot, sandbox and node identities")
	}
	return store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		rec, err := lockSnapshotRecordTx(tx, snapshotID)
		if err != nil {
			return err
		}
		if err := validateSnapshotNewUse(rec, time.Now()); err != nil {
			return err
		}
		now := time.Now()
		ref := normalizeSnapshotRuntimeRef(SnapshotRuntimeRefInfo{
			SnapshotID: snapshotID, SandboxID: sandboxID, NodeID: nodeID, NodeIP: nodeIP,
			BindingType: SnapshotRuntimeBindingCreatePending,
		})
		// A reservation is never an upsert: a reused sandbox identity must not
		// overwrite an existing operation's protection.
		return tx.Table(constants.SnapshotRuntimeActiveTableName).
			Create(newSnapshotRuntimeActiveModel(ref, now, &now, now)).Error
	})
}

func lockSnapshotRecordTx(tx *gorm.DB, snapshotID string) (*models.SnapshotRecord, error) {
	var rec models.SnapshotRecord
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("snapshot_id = ?", snapshotID).First(&rec).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, wrapSnapshotNotFound(snapshotID)
		}
		return nil, err
	}
	return &rec, nil
}

// fenceSnapshotCleanupTx shares the source-row lock with create reservation.
// Once DELETING is committed, no new reservation can enter the physical phase.
func fenceSnapshotCleanupTx(tx *gorm.DB, snapshotID string) error {
	rec, err := lockSnapshotRecordTx(tx, snapshotID)
	if err != nil {
		return err
	}
	if strings.EqualFold(rec.Status, StatusDeleting) || strings.EqualFold(rec.Status, StatusCreating) {
		return ErrTemplateAttemptInProgress
	}
	if err := rejectActiveSnapshotJobTx(tx, snapshotID); err != nil {
		return err
	}
	var refs int64
	if err := tx.Table(constants.SnapshotRuntimeActiveTableName).
		Where("snapshot_id = ?", snapshotID).Count(&refs).Error; err != nil {
		return err
	}
	if refs > 0 {
		return fmt.Errorf("%w: snapshot %s has %d runtime references", ErrTemplateInUse, snapshotID, refs)
	}
	return nil
}

func rejectActiveSnapshotJobTx(tx *gorm.DB, snapshotID string) error {
	var jobs int64
	if err := tx.Table(constants.TemplateImageJobTableName).
		Where("resource_id = ? AND operation IN ? AND status IN ?", snapshotID,
			[]string{JobOperationSnapshotCreate, JobOperationSnapshotRollback, JobOperationSnapshotDelete},
			[]string{JobStatusPending, JobStatusRunning}).Count(&jobs).Error; err != nil {
		return err
	}
	if jobs > 0 {
		return ErrTemplateAttemptInProgress
	}
	return nil
}
