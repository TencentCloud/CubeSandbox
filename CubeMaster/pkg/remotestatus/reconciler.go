// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package remotestatus writes the terminal remote_status of S3 / jfs rows
// that are still uploading (remote_status=inprogress). Watch settles a row
// as soon as its Cubelet reports the upload done, by holding a
// Snapshot.Status call open; the reconcile loop polls every row left over,
// e.g. after a Master restart. Multiple CubeMaster replicas may run both;
// updates use WHERE remote_status=inprogress so a replica that already
// finished the row is not overwritten.
package remotestatus

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	snapshotv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/snapshot/v1"
	"gorm.io/gorm"
)

const reconcileInterval = 5 * time.Second

const (
	// watchWaitReady is how long one Status call may wait on Cubelet.
	watchWaitReady = 10 * time.Second
	// watchLimit bounds a Watch; the reconcile loop takes over afterwards.
	watchLimit = 5 * time.Minute
	// watchRetryGap spaces out calls to a Cubelet that answers at once
	// because it does not know wait_ready_ms.
	watchRetryGap = 500 * time.Millisecond
)

type workItem struct {
	kind       string
	table      string
	snapshotID string
	hostIP     string
	backend    string
}

const (
	kindSnapshot = "snapshot"
	kindPause    = "pause"
)

var (
	startOnce    sync.Once
	queryStatus  = cubelet.SnapshotStatus
	cubeletAddr  = cubelet.GetCubeletAddr
	nowTimestamp = func() any { return gorm.Expr("CURRENT_TIMESTAMP") }
	watchDB      atomic.Pointer[gorm.DB]
)

// Start runs the inprogress poller until ctx is cancelled. Safe to call
// once per process; extra calls are ignored.
func Start(ctx context.Context, db *gorm.DB) {
	if db == nil {
		return
	}
	startOnce.Do(func() {
		watchDB.Store(db)
		go func() {
			for {
				ReconcileOnce(ctx, db)
				select {
				case <-ctx.Done():
					return
				case <-time.After(reconcileInterval):
				}
			}
		}()
	})
}

// ReconcileOnce lists inprogress S3 rows on both snapshot tables, queries
// Cubelet Status, and writes ready／failed only while the row is still
// inprogress.
func ReconcileOnce(ctx context.Context, db *gorm.DB) {
	if db == nil {
		return
	}
	items, err := listInProgress(ctx, db)
	if err != nil {
		log.G(ctx).Warnf("remote_status list inprogress failed: %v", err)
		return
	}
	for _, item := range items {
		if ctx.Err() != nil {
			return
		}
		settleItem(ctx, db, item, 0)
	}
}

// WatchPause settles the remote_status of a pause snapshot Pause just left
// inprogress, without waiting for the next reconcile tick.
func WatchPause(ctx context.Context, snapshotID, nodeIP, backend string) {
	watch(ctx, workItem{kind: kindPause, table: constants.PauseSnapshotTableName,
		snapshotID: snapshotID, hostIP: nodeIP, backend: backend})
}

// WatchSnapshot is WatchPause for a snapshot CommitSandbox just left
// inprogress.
func WatchSnapshot(ctx context.Context, snapshotID, nodeIP, backend string) {
	watch(ctx, workItem{kind: kindSnapshot, table: constants.SnapshotTableName,
		snapshotID: snapshotID, hostIP: nodeIP, backend: backend})
}

func watch(ctx context.Context, item workItem) {
	db := watchDB.Load()
	if db == nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	go func() {
		ctx, cancel := context.WithTimeout(ctx, watchLimit)
		defer cancel()
		for ctx.Err() == nil {
			start := time.Now()
			if settleItem(ctx, db, item, watchWaitReady) {
				return
			}
			select {
			case <-ctx.Done():
			case <-time.After(watchRetryGap - time.Since(start)):
			}
		}
	}()
}

func listInProgress(ctx context.Context, db *gorm.DB) ([]workItem, error) {
	var items []workItem
	var snaps []models.SnapshotRecord
	if err := db.WithContext(ctx).Table(constants.SnapshotTableName).
		Where("backend IN ? AND remote_status = ?", constants.RemoteTrackedBackends(), constants.RemoteStatusInProgress).
		Find(&snaps).Error; err != nil {
		return nil, err
	}
	for _, rec := range snaps {
		items = append(items, workItem{
			kind:       kindSnapshot,
			table:      constants.SnapshotTableName,
			snapshotID: rec.SnapshotID,
			hostIP:     rec.OriginNodeIP,
			backend:    rec.Backend,
		})
	}
	var pauses []models.PauseSnapshotRecord
	if err := db.WithContext(ctx).Table(constants.PauseSnapshotTableName).
		Where("backend IN ? AND remote_status = ?", constants.RemoteTrackedBackends(), constants.RemoteStatusInProgress).
		Find(&pauses).Error; err != nil {
		return nil, err
	}
	for _, rec := range pauses {
		items = append(items, workItem{
			kind:       kindPause,
			table:      constants.PauseSnapshotTableName,
			snapshotID: rec.SnapshotID,
			hostIP:     rec.NodeIP,
			backend:    rec.Backend,
		})
	}
	return items, nil
}

// settleItem asks Cubelet for the upload state of item, letting it hold the
// reply up to wait, and writes a terminal result. It reports whether the
// row needs no more queries: settled now, already settled elsewhere, or
// unqueryable.
func settleItem(ctx context.Context, db *gorm.DB, item workItem, wait time.Duration) bool {
	hostIP := strings.TrimSpace(item.hostIP)
	snapshotID := strings.TrimSpace(item.snapshotID)
	if hostIP == "" || snapshotID == "" {
		return true
	}
	queryCtx := ctx
	if wait > 0 {
		var cancel context.CancelFunc
		queryCtx, cancel = context.WithTimeout(ctx, wait+5*time.Second)
		defer cancel()
	}
	st, err := queryStatus(queryCtx, cubeletAddr(hostIP), &snapshotv1.StatusRequest{
		SnapshotId:  snapshotID,
		Backend:     item.backend,
		WaitReadyMs: int32(wait / time.Millisecond),
	})
	if err != nil || st == nil {
		log.G(ctx).Warnf("remote_status query %s %s on %s failed: %v", item.kind, snapshotID, hostIP, err)
		return false
	}
	next, ok := terminalRemoteStatus(st.GetState(), st.GetRemoteReady())
	if !ok {
		return false
	}
	res := db.WithContext(ctx).Table(item.table).
		Where("snapshot_id = ? AND remote_status = ?", snapshotID, constants.RemoteStatusInProgress).
		Updates(map[string]any{
			"remote_status": next,
			"updated_at":    nowTimestamp(),
		})
	if res.Error != nil {
		log.G(ctx).Warnf("remote_status update %s %s failed: %v", item.kind, snapshotID, res.Error)
		return false
	}
	if res.RowsAffected > 0 {
		log.G(ctx).Infof("remote_status %s %s -> %s", item.kind, snapshotID, next)
	}
	return true
}

func terminalRemoteStatus(state string, remoteReady bool) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case constants.RemoteStatusReady:
		return constants.RemoteStatusReady, true
	case constants.RemoteStatusFailed:
		return constants.RemoteStatusFailed, true
	case constants.RemoteStatusRunning, constants.RemoteStatusInProgress, constants.RemoteStatusPending:
		return "", false
	}
	if remoteReady {
		return constants.RemoteStatusReady, true
	}
	return "", false
}
