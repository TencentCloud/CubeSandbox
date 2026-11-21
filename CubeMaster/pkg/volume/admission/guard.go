// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package admission serializes Create and Delete admission on the volume row.
package admission

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync/atomic"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/volume/refcount"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrBusy = errors.New("volume is in use, being attached, or being deleted")

type CreateGuard struct {
	db   *gorm.DB
	rows []models.VolumeRecord
	used atomic.Bool
}

// BeginCreate reserves all named volumes before the RPC is sent. Calls for
// the same volume may overlap; no transaction lasts across the RPC.
func BeginCreate(ctx context.Context, db *gorm.DB, ids []string) (*CreateGuard, []models.VolumeRecord, error) {
	if len(ids) == 0 {
		return nil, nil, nil
	}
	ids = slices.Clone(ids)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	g := &CreateGuard{db: db}
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, id := range ids {
			var row models.VolumeRecord
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("volume_id = ?", id).Take(&row).Error; err != nil {
				return err
			}
			if row.VolumeID != id {
				return gorm.ErrRecordNotFound
			}
			if row.Deleting || row.InFlightCreates < 0 {
				return ErrBusy
			}
			if err := tx.Model(&row).UpdateColumn("in_flight_creates", gorm.Expr("in_flight_creates + 1")).Error; err != nil {
				return err
			}
			g.rows = append(g.rows, row)
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
// attempt. A failed transaction cannot be safely replayed against the counter.
func (g *CreateGuard) Finish(ctx context.Context, ext map[string][]byte, rolledBack bool) error {
	if !g.used.CompareAndSwap(false, true) {
		return errors.New("volume create guard already settled")
	}
	var events []refcount.Event
	if data, exists := ext[refcount.ExtInfoKey]; exists {
		if err := json.Unmarshal(data, &events); err != nil {
			return err
		}
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
	return g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, row := range g.rows {
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
		return nil
	})
}

// BeginDelete claims the exact row before the potentially slow driver call.
func BeginDelete(ctx context.Context, db *gorm.DB, id uint) (models.VolumeRecord, error) {
	var row models.VolumeRecord
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Take(&row, id).Error; err != nil {
			return err
		}
		if row.RefCount != 0 || row.InFlightCreates != 0 || row.Deleting {
			return ErrBusy
		}
		return tx.Model(&row).UpdateColumn("deleting", true).Error
	})
	return row, err
}
