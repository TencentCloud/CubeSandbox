//go:build integration

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package admission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxspec"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/volume/refcount"
	"github.com/tencentcloud/CubeSandbox/pkgs/cubedb/migrate"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestVolumeAdmissionRealDatabases(t *testing.T) {
	for _, tc := range []struct {
		name  string
		env   string
		open  func(string) gorm.Dialector
		count string
	}{
		{"mysql", "CUBEMASTER_DAO_TEST_MYSQL_DSN", mysql.Open, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE()"},
		{"postgres", "CUBEMASTER_DAO_TEST_POSTGRES_DSN", postgres.Open, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=current_schema()"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			require.NotEmpty(t, dsn, "dedicated empty SQL fixture required")
			db, err := gorm.Open(tc.open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			require.True(t, err == nil, "open dedicated SQL fixture failed")
			raw, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { _ = raw.Close() })
			raw.SetMaxOpenConns(4)
			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
			defer cancel()
			var tables int
			require.NoError(t, db.WithContext(ctx).Raw(tc.count).Scan(&tables).Error)
			require.Zero(t, tables, "refusing a nonempty database")
			require.NoError(t, migrate.Run(ctx, raw, tc.name, nil))

			for _, winner := range []string{"create", "delete", "delete_retry"} {
				t.Run(winner+"_locks_first", func(t *testing.T) {
					testVolumeRowContention(t, ctx, db, winner)
				})
			}
			t.Run("confirmed_rollback_releases_last_node_reference", func(t *testing.T) {
				row := models.VolumeRecord{VolumeID: "shared", Name: "shared", Driver: "fixture"}
				require.NoError(t, db.Create(&row).Error)
				failed, _, err := BeginCreate(ctx, db, []string{row.VolumeID})
				require.NoError(t, err)
				success, _, err := BeginCreate(ctx, db, []string{row.VolumeID})
				require.NoError(t, err)
				require.NoError(t, success.Finish(ctx, map[string][]byte{
					refcount.ExtInfoKey: []byte(`[{"volume_id":"shared","referenced":1}]`),
				}, false))
				// The successful sandbox left first, while the failed Create
				// still held the last local reference.
				_, err = BeginDelete(ctx, db, row.ID)
				require.ErrorIs(t, err, ErrBusy)
				require.NoError(t, failed.Finish(ctx, map[string][]byte{
					refcount.ExtInfoKey: []byte(`[{"volume_id":"shared","referenced":0}]`),
				}, true))
				settled, err := BeginDelete(ctx, db, row.ID)
				require.NoError(t, err)
				require.Zero(t, settled.RefCount)
				require.Zero(t, settled.InFlightCreates)
			})
			t.Run("rollback_arrives_before_claimed_create_report", func(t *testing.T) {
				testRollbackBeforeCreateReport(t, db)
			})
			t.Run("runtime_attach_settlement", func(t *testing.T) {
				testRuntimeAttachSettlement(t, db)
			})
			t.Run("resume_volume_identity", func(t *testing.T) {
				testResumeVolumeIdentity(t, ctx, db)
			})
			t.Run("runtime_attach_marker_and_reservation", func(t *testing.T) {
				testRuntimeAttachMarker(t, ctx, db)
			})
			t.Run("runtime_attach_durable_contention", func(t *testing.T) {
				testRuntimeAttachDurableContention(t, ctx, db)
			})
			t.Run("runtime_attach_mount_capacity", func(t *testing.T) {
				testRuntimeAttachMountCapacity(t, ctx, db)
			})
		})
	}
}

func testRuntimeAttachMountCapacity(t *testing.T, ctx context.Context, db *gorm.DB) {
	t.Helper()
	const mysqlTextCapacity = 65_535
	suffix := strings.Repeat("/"+strings.Repeat("a", 240), 16)
	mounts := make([]sandboxspec.RuntimeVolumeMount, 16, 17)
	for i := range mounts {
		id := fmt.Sprintf("existing-%02d", i)
		mounts[i] = sandboxspec.RuntimeVolumeMount{
			VolumeID: id, VolumeRecordID: uint(i + 1), ContainerPath: "/" + id + suffix, Readonly: true,
		}
	}
	current, err := json.Marshal(mounts)
	require.NoError(t, err)
	require.Less(t, len(current), mysqlTextCapacity)

	for _, size := range []int{mysqlTextCapacity, mysqlTextCapacity + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			id := fmt.Sprintf("capacity-%d", size)
			volume := models.VolumeRecord{VolumeID: id, Name: id, Driver: "fixture"}
			spec := models.SandboxSpec{SandboxID: id, RequestJSON: "{}"}
			require.NoError(t, db.WithContext(ctx).Create(&volume).Error)
			require.NoError(t, db.WithContext(ctx).Create(&spec).Error)
			require.NoError(t, db.WithContext(ctx).Model(&spec).UpdateColumn("runtime_volume_mounts", string(current)).Error)
			request := runtimeAttachRequestForTest(spec, volume, id)
			mount := sandboxspec.RuntimeVolumeMount{
				VolumeID: volume.VolumeID, VolumeRecordID: volume.ID, ContainerPath: request.ContainerPath, Readonly: request.Readonly,
			}
			payload, err := json.Marshal(append(mounts, mount))
			require.NoError(t, err)
			padding := size - len(payload)
			require.Positive(t, padding)
			mount.ContainerPath += strings.Repeat("/a", padding/2) + strings.Repeat("a", padding%2)
			require.LessOrEqual(t, len(mount.ContainerPath), 4096)
			request.ContainerPath = mount.ContainerPath
			payload, err = json.Marshal(append(mounts, mount))
			require.NoError(t, err)
			require.Len(t, payload, size)

			guard, admitted, err := BeginRuntimeAttach(ctx, db, request, nil)
			requestKey := models.RuntimeVolumeAttachKey(spec.SandboxID, request.RequestID)
			var stored models.RuntimeVolumeAttach
			wantPayload := string(payload)
			var wantRefs int64 = 1
			if db.Dialector.Name() == "mysql" && size > mysqlTextCapacity {
				require.ErrorIs(t, err, sandboxspec.ErrRuntimeVolumeMountsTooLarge)
				require.Nil(t, guard)
				require.Zero(t, admitted.ID)
				require.ErrorIs(t, db.WithContext(ctx).Take(&stored, "request_key = ?", requestKey).Error, gorm.ErrRecordNotFound)
				wantPayload = string(current)
				wantRefs = 0
			} else {
				require.NoError(t, err)
				require.Equal(t, volume.ID, admitted.ID)
				report, err := json.Marshal([]refcount.Event{{VolumeID: volume.VolumeID, Referenced: 1}})
				require.NoError(t, err)
				require.NoError(t, guard.FinishRuntimeAttach(ctx, map[string][]byte{refcount.ExtInfoKey: report}, spec.SandboxID, spec.ID, mount))
				require.NoError(t, db.WithContext(ctx).Take(&stored, "request_key = ?", requestKey).Error)
				require.Equal(t, models.RuntimeAttachPublished, stored.Outcome)
			}
			require.NoError(t, db.WithContext(ctx).Take(&volume, volume.ID).Error)
			require.Zero(t, volume.InFlightCreates)
			require.Equal(t, wantRefs, volume.RefCount)
			var storedSpec models.SandboxSpec
			require.NoError(t, db.WithContext(ctx).Take(&storedSpec, spec.ID).Error)
			require.Nil(t, storedSpec.PendingRuntimeAttach)
			require.NotNil(t, storedSpec.RuntimeVolumeMounts)
			require.Equal(t, wantPayload, *storedSpec.RuntimeVolumeMounts)
		})
	}
}

func testRuntimeAttachDurableContention(t *testing.T, parent context.Context, db *gorm.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	volume := models.VolumeRecord{VolumeID: "durable-data", Name: "durable-data", Driver: "fixture"}
	spec := models.SandboxSpec{SandboxID: "durable-sandbox", RequestJSON: "{}"}
	require.NoError(t, db.WithContext(ctx).Create(&volume).Error)
	require.NoError(t, db.WithContext(ctx).Create(&spec).Error)
	request := runtimeAttachRequestForTest(spec, volume, "stable")
	requestKey := models.RuntimeVolumeAttachKey(spec.SandboxID, request.RequestID)

	type observeQuery struct{}
	seenQuery := make(chan struct{}, 1)
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register("observe_runtime_attach_racer", func(tx *gorm.DB) {
		if tx.Statement.Context.Value(observeQuery{}) == true && tx.Statement.Table == (models.VolumeRecord{}).TableName() {
			select {
			case seenQuery <- struct{}{}:
			default:
			}
		}
	}))
	defer db.Callback().Query().Remove("observe_runtime_attach_racer")
	race := func(entered, release chan struct{}, first, second func(context.Context) error) error {
		results, later := make(chan error, 1), make(chan error, 1)
		defer close(release)
		go func() { results <- first(ctx) }()
		select {
		case <-entered:
		case err := <-results:
			t.Fatalf("first transaction did not hold its write: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		go func() { later <- second(context.WithValue(ctx, observeQuery{}, true)) }()
		select {
		case <-seenQuery:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		select {
		case err := <-later:
			t.Fatalf("second transaction escaped the held volume row lock: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		release <- struct{}{}
		require.NoError(t, <-results)
		return <-later
	}

	entered, release := make(chan struct{}), make(chan struct{}, 1)
	err := race(entered, release, func(ctx context.Context) error {
		_, _, err := BeginRuntimeAttach(ctx, db, request, func(*gorm.DB, string) error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		return err
	}, func(ctx context.Context) error {
		_, _, err := BeginRuntimeAttach(ctx, db, request, nil)
		return err
	})
	require.ErrorIs(t, err, sandboxspec.ErrRuntimeAttachPending)
	var stored models.RuntimeVolumeAttach
	require.NoError(t, db.WithContext(ctx).Take(&stored, "request_key = ?", requestKey).Error)
	require.Equal(t, models.RuntimeAttachPending, stored.Outcome)
	require.NoError(t, db.WithContext(ctx).Take(&volume, volume.ID).Error)
	require.EqualValues(t, 1, volume.InFlightCreates)
	otherGuard, _, err := BeginCreate(ctx, db, []string{volume.VolumeID})
	require.NoError(t, err)

	type holdSettlement struct{}
	entered, release = make(chan struct{}), make(chan struct{}, 1)
	require.NoError(t, db.Callback().Update().After("gorm:update").Register("hold_runtime_attach_receipt", func(tx *gorm.DB) {
		if tx.Statement.Context.Value(holdSettlement{}) == true && tx.Statement.Table == (models.RuntimeVolumeAttach{}).TableName() &&
			tx.Error == nil && tx.RowsAffected == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				_ = tx.AddError(ctx.Err())
			}
		}
	}))
	defer db.Callback().Update().Remove("hold_runtime_attach_receipt")
	report := map[string][]byte{refcount.ExtInfoKey: []byte(`[{"volume_id":"durable-data","referenced":1}]`)}
	err = race(entered, release, func(ctx context.Context) error {
		return SettleRuntimeAttach(context.WithValue(ctx, holdSettlement{}, true), db, stored, report, models.RuntimeAttachPublished)
	}, func(ctx context.Context) error {
		return SettleRuntimeAttach(ctx, db, stored, report, models.RuntimeAttachPublished)
	})
	require.ErrorContains(t, err, "marker changed")
	require.NoError(t, db.WithContext(ctx).Take(&volume, volume.ID).Error)
	require.EqualValues(t, 1, volume.InFlightCreates)
	require.EqualValues(t, 1, volume.RefCount)
	require.NoError(t, db.WithContext(ctx).Take(&stored, "request_key = ?", requestKey).Error)
	require.Equal(t, models.RuntimeAttachPublished, stored.Outcome)
	require.NoError(t, otherGuard.Finish(ctx, nil, false))

	// The durable key still excludes this exact request after its marker has
	// cleared, and preserves a new request which differs only in case.
	_, _, err = BeginRuntimeAttach(ctx, db, request, nil)
	require.Error(t, err)
	require.NoError(t, db.WithContext(ctx).Take(&spec, spec.ID).Error)
	require.Nil(t, spec.PendingRuntimeAttach)
	require.NoError(t, db.WithContext(ctx).Take(&volume, volume.ID).Error)
	require.Zero(t, volume.InFlightCreates)
	request.RequestID = "STABLE"
	guard, _, err := BeginRuntimeAttach(ctx, db, request, nil)
	require.NoError(t, err)
	require.NoError(t, guard.Finish(ctx, nil, false))
	var caseVariant models.RuntimeVolumeAttach
	require.NoError(t, db.WithContext(ctx).Take(&caseVariant, "request_key = ?", models.RuntimeVolumeAttachKey(spec.SandboxID, request.RequestID)).Error)
	require.Equal(t, request.RequestID, caseVariant.RequestID)
	require.Equal(t, models.RuntimeAttachNotStarted, caseVariant.Outcome)
}

func testRuntimeAttachMarker(t *testing.T, ctx context.Context, db *gorm.DB) {
	t.Helper()
	row := models.VolumeRecord{VolumeID: "marked-data", Name: "marked-data", Driver: "fixture"}
	spec := models.SandboxSpec{SandboxID: "marked-sandbox", InstanceType: "cubebox", RequestJSON: "{}"}
	require.NoError(t, db.Create(&row).Error)
	require.NoError(t, db.Create(&spec).Error)

	activeSnapshot := errors.New("active snapshot job")
	request := runtimeAttachRequestForTest(spec, row, "marker")
	_, _, err := BeginRuntimeAttach(ctx, db, request, func(tx *gorm.DB, sandboxID string) error {
		var marked models.SandboxSpec
		require.NoError(t, tx.Where("sandbox_id = ?", sandboxID).Take(&marked).Error)
		require.NotNil(t, marked.PendingRuntimeAttach)
		var reserved models.VolumeRecord
		require.NoError(t, tx.Take(&reserved, row.ID).Error)
		require.EqualValues(t, 1, reserved.InFlightCreates)
		return activeSnapshot
	})
	require.ErrorIs(t, err, activeSnapshot)
	require.NoError(t, db.Take(&spec, spec.ID).Error)
	require.Nil(t, spec.PendingRuntimeAttach)
	require.NoError(t, db.Take(&row, row.ID).Error)
	require.Zero(t, row.InFlightCreates)

	guard, admitted, err := BeginRuntimeAttach(ctx, db, request, nil)
	require.NoError(t, err)
	require.Equal(t, row.ID, admitted.ID)
	require.NoError(t, db.Take(&spec, spec.ID).Error)
	require.NotNil(t, spec.PendingRuntimeAttach)
	_, _, err = BeginRuntimeAttach(ctx, db, request, nil)
	require.ErrorIs(t, err, sandboxspec.ErrRuntimeAttachPending)
	require.NoError(t, db.Take(&row, row.ID).Error)
	require.EqualValues(t, 1, row.InFlightCreates)
	_, err = BeginDelete(ctx, db, row.ID)
	require.ErrorIs(t, err, ErrBusy)

	mount := sandboxspec.RuntimeVolumeMount{VolumeID: row.VolumeID, VolumeRecordID: row.ID, ContainerPath: "/data", Readonly: true}
	ext := map[string][]byte{refcount.ExtInfoKey: []byte(`[{"volume_id":"marked-data","referenced":1}]`)}
	require.NoError(t, guard.FinishRuntimeAttach(ctx, ext, spec.SandboxID, spec.ID, mount))
	require.NoError(t, db.Take(&spec, spec.ID).Error)
	require.Nil(t, spec.PendingRuntimeAttach)
	require.NotNil(t, spec.RuntimeVolumeMounts)
	require.NoError(t, db.Take(&row, row.ID).Error)
	require.Zero(t, row.InFlightCreates)
	require.EqualValues(t, 1, row.RefCount)
}

func testResumeVolumeIdentity(t *testing.T, ctx context.Context, db *gorm.DB) {
	t.Helper()
	static := models.VolumeRecord{VolumeID: "a-resume-work", Name: "a-resume-work", Driver: "fixture", InFlightCreates: 1}
	original := models.VolumeRecord{VolumeID: "z-resume-data", Name: "z-resume-data", Driver: "fixture"}
	require.NoError(t, db.Create(&static).Error)
	require.NoError(t, db.Create(&original).Error)
	require.NoError(t, db.Delete(&original).Error)
	replacement := models.VolumeRecord{VolumeID: original.VolumeID, Name: original.Name, Driver: "fixture"}
	require.NoError(t, db.Create(&replacement).Error)
	require.NotEqual(t, original.ID, replacement.ID)

	guard, rows, err := BeginCreateWithExpectedRows(ctx, db, []string{static.VolumeID}, map[string]uint{original.VolumeID: original.ID})
	require.ErrorIs(t, err, ErrVolumeReplaced)
	require.Nil(t, guard)
	require.Empty(t, rows)
	require.NoError(t, db.Take(&static, static.ID).Error)
	require.EqualValues(t, 1, static.InFlightCreates)
	require.NoError(t, db.Take(&replacement, replacement.ID).Error)
	require.Zero(t, replacement.InFlightCreates)

	guard, rows, err = BeginCreateWithExpectedRows(ctx, db, nil, map[string]uint{replacement.VolumeID: replacement.ID})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, replacement.ID, rows[0].ID)
	_, err = BeginDelete(ctx, db, replacement.ID)
	require.ErrorIs(t, err, ErrBusy)
	require.NoError(t, guard.Finish(ctx, nil, false))
	require.NoError(t, db.Take(&replacement, replacement.ID).Error)
	require.Zero(t, replacement.InFlightCreates)
}

func testRuntimeAttachSettlement(t *testing.T, db *gorm.DB) {
	t.Helper()
	for _, tc := range []struct {
		name    string
		deleted bool
	}{
		{name: "commit"},
		{name: "rollback", deleted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			id := "runtime-" + tc.name
			row := models.VolumeRecord{VolumeID: id, Name: id, Driver: "fixture"}
			require.NoError(t, db.WithContext(ctx).Create(&row).Error)
			spec := models.SandboxSpec{SandboxID: id, InstanceType: "cubebox", RequestJSON: `{"instance_type":"cubebox"}`}
			require.NoError(t, db.WithContext(ctx).Create(&spec).Error)

			guard, _, err := BeginCreate(ctx, db, []string{id})
			require.NoError(t, err)
			if tc.deleted {
				require.NoError(t, db.WithContext(ctx).Delete(&spec).Error)
			}
			ext := map[string][]byte{
				refcount.ExtInfoKey: []byte(`[{"volume_id":"` + id + `","referenced":1}]`),
			}
			mount := sandboxspec.RuntimeVolumeMount{VolumeID: id, VolumeRecordID: row.ID, ContainerPath: "/data", Readonly: true}
			wrong := mount
			wrong.VolumeRecordID++
			require.ErrorContains(t, guard.FinishRuntimeAttach(ctx, ext, id, spec.ID, wrong), "reserved volume row")
			finishErr := guard.FinishRuntimeAttach(ctx, ext, id, spec.ID, mount)
			var settled models.VolumeRecord
			require.NoError(t, db.WithContext(ctx).Take(&settled, row.ID).Error)
			var stored models.SandboxSpec
			require.NoError(t, db.WithContext(ctx).Unscoped().Take(&stored, spec.ID).Error)
			if tc.deleted {
				require.ErrorIs(t, finishErr, sandboxspec.ErrSandboxSpecNotFound)
				require.Zero(t, settled.RefCount)
				require.EqualValues(t, 1, settled.InFlightCreates)
				require.Nil(t, stored.RuntimeVolumeMounts)
			} else {
				require.NoError(t, finishErr)
				require.EqualValues(t, 1, settled.RefCount)
				require.Zero(t, settled.InFlightCreates)
				require.NotNil(t, stored.RuntimeVolumeMounts)
				require.Contains(t, *stored.RuntimeVolumeMounts, `"container_path":"/data"`)
			}
			_, err = BeginDelete(ctx, db, row.ID)
			require.ErrorIs(t, err, ErrBusy)
			require.ErrorContains(t, guard.FinishRuntimeAttach(ctx, ext, id, spec.ID, mount), "already settled")
		})
	}
}

func testVolumeRowContention(t *testing.T, parent context.Context, db *gorm.DB, winner string) {
	ctx, cancel := context.WithTimeout(parent, 8*time.Second)
	defer cancel()
	row := models.VolumeRecord{VolumeID: "first-" + winner, Name: "first-" + winner, Driver: "fixture"}
	if winner == "delete_retry" {
		row.Deleting, row.DeleteRetryable = true, true
	}
	require.NoError(t, db.Create(&row).Error)
	type marker struct{}
	firstWrite, unblock, otherQuery := make(chan struct{}), make(chan struct{}), make(chan struct{})
	require.NoError(t, db.Callback().Update().After("gorm:update").Register("pause_winner", func(tx *gorm.DB) {
		if tx.Statement.Context.Value(marker{}) == "winner" && tx.Error == nil && tx.RowsAffected == 1 {
			close(firstWrite)
			select {
			case <-unblock:
			case <-ctx.Done():
				tx.AddError(ctx.Err())
			}
		}
	}))
	defer db.Callback().Update().Remove("pause_winner")
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register("observe_loser", func(tx *gorm.DB) {
		if tx.Statement.Context.Value(marker{}) == "loser" {
			close(otherQuery)
		}
	}))
	defer db.Callback().Query().Remove("observe_loser")

	create := func(ctx context.Context) error {
		_, _, err := BeginCreate(ctx, db, []string{row.VolumeID})
		return err
	}
	del := func(ctx context.Context) error {
		_, err := BeginDelete(ctx, db, row.ID)
		return err
	}
	first, other := create, del
	switch winner {
	case "delete":
		first, other = del, create
	case "delete_retry":
		first, other = del, del
	}
	firstResult, otherResult := make(chan error, 1), make(chan error, 1)
	go func() { firstResult <- first(context.WithValue(ctx, marker{}, "winner")) }()
	select {
	case <-firstWrite:
	case err := <-firstResult:
		t.Fatalf("winner returned before its successful SQL write: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	go func() { otherResult <- other(context.WithValue(ctx, marker{}, "loser")) }()
	select {
	case <-otherQuery:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case err := <-otherResult:
		close(unblock)
		<-firstResult
		t.Fatalf("second transaction escaped the uncommitted row lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(unblock)
	require.NoError(t, <-firstResult)
	require.ErrorIs(t, <-otherResult, ErrBusy)
	var after models.VolumeRecord
	require.NoError(t, db.Take(&after, row.ID).Error)
	require.Zero(t, after.RefCount)
	if winner == "create" {
		require.EqualValues(t, 1, after.InFlightCreates)
		require.False(t, after.Deleting)
	} else {
		require.Zero(t, after.InFlightCreates)
		require.True(t, after.Deleting)
		require.False(t, after.DeleteRetryable)
	}
}
