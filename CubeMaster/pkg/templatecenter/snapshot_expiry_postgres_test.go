// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"gorm.io/gorm"
)

// Real row locks are essential here; SQLite cannot verify cross-Master fencing.
func TestSnapshotCreateCleanupPostgreSQLRace(t *testing.T) {
	env := newPGDockerEnv(t)
	defer env.teardown()
	oldDB := store.db
	store.db = openMigratedPostgresGORM(t, env)
	defer func() { store.db = oldDB }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("snap-race-%d-%d", time.Now().UnixNano(), i)
		require.NoError(t, store.db.Create(&models.SnapshotRecord{SnapshotID: id, Status: StatusReady}).Error)
		start := make(chan struct{})
		createDone, cleanupDone := make(chan error, 1), make(chan error, 1)
		go func() {
			<-start
			createDone <- ReserveSnapshotCreate(ctx, id, id+"-sandbox", "node", "")
		}()
		go func() {
			<-start
			cleanupDone <- store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				if err := fenceSnapshotCleanupTx(tx, id); err != nil {
					return err
				}
				return updateSnapshotFieldsTx(tx, id, map[string]any{"status": StatusDeleting})
			})
		}()
		close(start)
		createErr, cleanupErr := <-createDone, <-cleanupDone
		if createErr == nil {
			require.ErrorIs(t, cleanupErr, ErrTemplateInUse, "cleanup must see the committed reservation")
		} else {
			require.NoError(t, cleanupErr)
			require.ErrorIs(t, createErr, ErrSnapshotNotFound, "create must see committed DELETING state")
		}
	}
}
