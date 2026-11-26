// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package admission serializes Create and Delete admission on the volume row.
package admission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxspec"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/volume/refcount"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrBusy           = errors.New("volume is in use, being attached, or being deleted")
	ErrVolumeReplaced = errors.New("original volume was replaced")
)

type CreateGuard struct {
	db            *gorm.DB
	rows          []models.VolumeRecord
	runtimeAttach *models.RuntimeVolumeAttach
	used          atomic.Bool
}

// BeginCreate reserves all named volumes before the RPC is sent. Calls for
// the same volume may overlap; no transaction lasts across the RPC.
func BeginCreate(ctx context.Context, db *gorm.DB, ids []string) (*CreateGuard, []models.VolumeRecord, error) {
	return BeginCreateWithExpectedRows(ctx, db, ids, nil)
}

// BeginCreateWithExpectedRows also reserves the volumes in expectedRows and
// verifies their record IDs under the same locks as the counter updates.
func BeginCreateWithExpectedRows(ctx context.Context, db *gorm.DB, ids []string, expectedRows map[string]uint) (*CreateGuard, []models.VolumeRecord, error) {
	return beginCreate(ctx, db, ids, expectedRows, nil, nil)
}

// BeginRuntimeAttach reserves one volume and saves its request and marker
// before dispatch. The caller holds the sandbox lifecycle lock.
// checkSandbox runs in the reservation transaction after the spec row is locked.
func BeginRuntimeAttach(ctx context.Context, db *gorm.DB, request models.RuntimeVolumeAttach, checkSandbox func(*gorm.DB, string) error) (*CreateGuard, models.VolumeRecord, error) {
	if request.SandboxID == "" || request.RequestID == "" || request.SpecRowID == 0 || request.VolumeID == "" {
		return nil, models.VolumeRecord{}, errors.New("runtime attach requires request, sandbox, spec row and volume IDs")
	}
	request.RequestKey = models.RuntimeVolumeAttachKey(request.SandboxID, request.RequestID)
	request.ClaimToken = uuid.NewString()
	request.Outcome = models.RuntimeAttachPending
	g, rows, err := beginCreate(ctx, db, []string{request.VolumeID}, nil, &request, checkSandbox)
	if err != nil {
		return nil, models.VolumeRecord{}, err
	}
	return g, rows[0], nil
}

func beginCreate(ctx context.Context, db *gorm.DB, ids []string, expectedRows map[string]uint, claim *models.RuntimeVolumeAttach, checkSandbox func(*gorm.DB, string) error) (*CreateGuard, []models.VolumeRecord, error) {
	if len(ids) == 0 && len(expectedRows) == 0 {
		return nil, nil, nil
	}
	ids = slices.Clone(ids)
	for id := range expectedRows {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	g := &CreateGuard{db: db, runtimeAttach: claim}
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, id := range ids {
			var row models.VolumeRecord
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("volume_id = ?", id).Take(&row).Error; err != nil {
				return err
			}
			if row.VolumeID != id {
				return gorm.ErrRecordNotFound
			}
			if expected, ok := expectedRows[id]; ok && row.ID != expected {
				return fmt.Errorf("%w: %s", ErrVolumeReplaced, id)
			}
			if row.Deleting || row.InFlightCreates < 0 {
				return ErrBusy
			}
			if err := tx.Model(&row).UpdateColumn("in_flight_creates", gorm.Expr("in_flight_creates + 1")).Error; err != nil {
				return err
			}
			g.rows = append(g.rows, row)
		}
		if claim != nil {
			if err := sandboxspec.ClaimRuntimeAttachInTx(tx, claim.SandboxID, claim.SpecRowID, claim.ClaimToken); err != nil {
				return err
			}
			if checkSandbox != nil {
				if err := checkSandbox(tx, claim.SandboxID); err != nil {
					return err
				}
			}
			claim.VolumeRecordID = g.rows[0].ID
			if err := sandboxspec.CheckRuntimeVolumeMountCapacityInTx(tx, claim.SandboxID, claim.SpecRowID, sandboxspec.RuntimeVolumeMount{
				VolumeID: claim.VolumeID, VolumeRecordID: claim.VolumeRecordID, ContainerPath: claim.ContainerPath, Readonly: claim.Readonly,
			}); err != nil {
				return err
			}
			return tx.Create(claim).Error
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return g, slices.Clone(g.rows), nil
}

// Finish removes this admission and applies any node reference changes in one
// transaction. Call only for success, confirmed rollback, or an unstarted
// attempt. Replay only when the database definitely aborted the transaction.
func (g *CreateGuard) Finish(ctx context.Context, ext map[string][]byte, rolledBack bool) error {
	return g.finish(ctx, ext, rolledBack, nil)
}

// FinishRuntimeAttach settles one reserved volume and records its confirmed
// mount atomically. The caller verifies guest publication and owns ordering
// with the sandbox lifecycle from dispatch through settlement.
func (g *CreateGuard) FinishRuntimeAttach(ctx context.Context, ext map[string][]byte, sandboxID string, specRowID uint, mount sandboxspec.RuntimeVolumeMount) error {
	if g == nil || len(g.rows) != 1 || g.rows[0].VolumeID != mount.VolumeID || g.rows[0].ID != mount.VolumeRecordID {
		return errors.New("runtime volume mount does not match the reserved volume row")
	}
	if claim := g.runtimeAttach; claim != nil && (claim.SandboxID != sandboxID || claim.SpecRowID != specRowID ||
		claim.ContainerPath != mount.ContainerPath || claim.Readonly != mount.Readonly) {
		return errors.New("runtime volume mount does not match the reserved spec row")
	}
	return g.finish(ctx, ext, false, func(tx *gorm.DB) error {
		return sandboxspec.RecordRuntimeVolumeMountInTx(tx, sandboxID, specRowID, mount)
	})
}

// SettleRuntimeAttach reconstructs the original guard from a durable request.
// The caller supplies a terminal outcome verified against the original binding.
func SettleRuntimeAttach(ctx context.Context, db *gorm.DB, request models.RuntimeVolumeAttach, ext map[string][]byte, outcome string) error {
	if request.Outcome != models.RuntimeAttachPending || request.VolumeRecordID == 0 || request.SpecRowID == 0 || request.ClaimToken == "" ||
		request.RequestKey != models.RuntimeVolumeAttachKey(request.SandboxID, request.RequestID) {
		return errors.New("runtime attach does not own a pending reservation")
	}
	guard := &CreateGuard{
		db:            db,
		rows:          []models.VolumeRecord{{ID: request.VolumeRecordID, VolumeID: request.VolumeID}},
		runtimeAttach: &request,
	}
	switch outcome {
	case models.RuntimeAttachPublished:
		return guard.FinishRuntimeAttach(ctx, ext, request.SandboxID, request.SpecRowID, sandboxspec.RuntimeVolumeMount{
			VolumeID: request.VolumeID, VolumeRecordID: request.VolumeRecordID, ContainerPath: request.ContainerPath, Readonly: request.Readonly,
		})
	case models.RuntimeAttachNotStarted:
		if _, reported := ext[refcount.ExtInfoKey]; reported {
			return errors.New("unstarted runtime attach cannot report volume references")
		}
		return guard.Finish(ctx, nil, false)
	case models.RuntimeAttachRolledBack:
		return guard.Finish(ctx, ext, true)
	default:
		return errors.New("runtime attach outcome is not terminal")
	}
}

func (g *CreateGuard) finish(ctx context.Context, ext map[string][]byte, rolledBack bool, afterSettle func(*gorm.DB) error) error {
	if !g.used.CompareAndSwap(false, true) {
		return errors.New("volume create guard already settled")
	}
	var events []refcount.Event
	if data, exists := ext[refcount.ExtInfoKey]; exists {
		if err := json.Unmarshal(data, &events); err != nil {
			return err
		}
	}
	if g.runtimeAttach != nil && afterSettle == nil && !rolledBack && len(events) != 0 {
		return errors.New("runtime attach reference report requires a confirmed mount")
	}
	expectedReference := 1
	reportDelta := 1
	if rolledBack {
		expectedReference = 0
		reportDelta = -1
	}
	changes := make(map[string]refcount.Event, len(events))
	for _, event := range events {
		_, duplicate := changes[event.VolumeID]
		if event.Referenced != expectedReference || duplicate ||
			!slices.ContainsFunc(g.rows, func(row models.VolumeRecord) bool { return row.VolumeID == event.VolumeID }) {
			return errors.New("invalid volume create reference report")
		}
		changes[event.VolumeID] = event
	}
	settle := func(tx *gorm.DB) error {
		for _, row := range g.rows {
			if g.runtimeAttach != nil {
				var current models.VolumeRecord
				if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
					Select("id", "volume_id").Take(&current, row.ID).Error; err != nil {
					return err
				}
				if current.VolumeID != row.VolumeID {
					return ErrVolumeReplaced
				}
			}
			event, reported := changes[row.VolumeID]
			delta := 0
			if reported {
				delta = reportDelta
			}
			target := tx.Model(&row).Where("in_flight_creates > 0 AND deleting = ?", false)
			if delta == -1 {
				// A claimed +1 can arrive later while another Create is
				// still reserved. This rollback releases only its own reservation.
				target = target.Where("(refcount > 0 OR (? AND in_flight_creates > 1))", event.CreateReportClaimed)
			}
			r := target.UpdateColumns(map[string]any{
				"in_flight_creates": gorm.Expr("in_flight_creates - 1"),
				"refcount":          gorm.Expr("refcount + ?", delta),
			})
			if r.Error != nil {
				return r.Error
			}
			if r.RowsAffected != 1 {
				return ErrBusy
			}
		}
		if afterSettle != nil {
			if err := afterSettle(tx); err != nil {
				return err
			}
		}
		if claim := g.runtimeAttach; claim != nil {
			if err := sandboxspec.ClearRuntimeAttachInTx(tx, claim.SandboxID, claim.SpecRowID, claim.ClaimToken); err != nil {
				return err
			}
			var persisted models.RuntimeVolumeAttach
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("request_key = ?", claim.RequestKey).Take(&persisted).Error; err != nil {
				return err
			}
			if persisted != *claim {
				return errors.New("runtime attach request changed before settlement")
			}
			outcome := models.RuntimeAttachNotStarted
			if rolledBack {
				outcome = models.RuntimeAttachRolledBack
			} else if afterSettle != nil {
				outcome = models.RuntimeAttachPublished
			}
			result := tx.Model(&models.RuntimeVolumeAttach{}).
				Where("request_key = ? AND claim_token = ? AND outcome = ?", claim.RequestKey, claim.ClaimToken, models.RuntimeAttachPending).
				UpdateColumn("outcome", outcome)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return errors.New("runtime attach request changed before settlement")
			}
		}
		return nil
	}
	db := g.db.WithContext(ctx)
	var bodyErr error
	err := db.Transaction(func(tx *gorm.DB) error {
		bodyErr = settle(tx)
		return bodyErr
	})
	// A Begin or Commit error never qualifies. In particular, a lost Commit
	// reply could mean the counter already reflects this receipt.
	if err != nil && ctx.Err() == nil && transactionAborted(bodyErr) {
		return db.Transaction(settle)
	}
	return err
}

func transactionAborted(err error) bool {
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) && myErr.Number == 1213 {
		return true
	}
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "40P01" || pgErr.Code == "40001")
}

// BeginDelete claims the exact row and returns its state before this attempt.
func BeginDelete(ctx context.Context, db *gorm.DB, id uint) (models.VolumeRecord, error) {
	var row models.VolumeRecord
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Take(&row, id).Error; err != nil {
			return err
		}
		if row.RefCount != 0 || row.InFlightCreates != 0 || (row.Deleting && !row.DeleteRetryable) {
			return ErrBusy
		}
		return tx.Model(&models.VolumeRecord{}).Where("id = ?", row.ID).UpdateColumns(map[string]any{
			"deleting":         true,
			"delete_retryable": false,
		}).Error
	})
	return row, err
}
