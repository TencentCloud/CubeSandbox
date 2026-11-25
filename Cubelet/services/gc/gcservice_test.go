// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package gc

import (
	"context"
	"testing"
	"time"

	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
)

type gcBoxStub struct {
	cb  *cubeboxstore.CubeBox
	err error
}

func (s gcBoxStub) Init(context.Context) error { return nil }
func (s gcBoxStub) Get(context.Context, string) (*cubeboxstore.CubeBox, error) {
	return s.cb, s.err
}
func (s gcBoxStub) FindContainerOfCubebox(context.Context, string) (*cubeboxstore.Container, *cubeboxstore.CubeBox, error) {
	return nil, nil, nil
}
func (s gcBoxStub) List() []*cubeboxstore.CubeBox { return nil }
func (s gcBoxStub) IsImageInUse(string) (bool, error) {
	return false, nil
}
func (s gcBoxStub) Save(context.Context, *cubeboxstore.CubeBox, ...cubes.UpdateCubeboxOpt) error {
	return nil
}
func (s gcBoxStub) SyncByID(context.Context, string, ...cubes.UpdateCubeboxOpt) error {
	return nil
}
func (s gcBoxStub) Delete(context.Context, *cubes.DeleteOption) error { return nil }

func TestAllowCleanupKeepsLiveSandbox(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := &local{config: &GCConfig{RootPath: dir}}
	if err := store.initDb(); err != nil {
		t.Fatal(err)
	}
	if err := store.saveSandBoxInfo(&sandBoxInfo{SandboxID: "sb-live", Namespace: "default"}); err != nil {
		t.Fatal(err)
	}
	svc := &gcService{gc: store}

	now := time.Now()
	marked := &cubeboxstore.CubeBox{}
	marked.ID = "sb-marked"
	marked.UserMarkDeletedTime = &now
	svc.gc.cubeboxManger = gcBoxStub{cb: marked}
	if !svc.allowCleanup(ctx, "sb-live") {
		t.Fatal("marked cubebox should still be cleaned")
	}
	left, err := store.readAll()
	if err != nil || len(left) != 1 {
		t.Fatalf("marked cleanup dropped the dirty record: %v %+v", err, left)
	}

	svc.gc.cubeboxManger = gcBoxStub{err: utils.ErrorKeyNotFound}
	if !svc.allowCleanup(ctx, "sb-live") {
		t.Fatal("missing cubebox should still be cleaned")
	}
	left, err = store.readAll()
	if err != nil || len(left) != 1 {
		t.Fatalf("missing cleanup dropped the dirty record: %v %+v", err, left)
	}

	live := &cubeboxstore.CubeBox{}
	live.ID = "sb-live"
	svc.gc.cubeboxManger = gcBoxStub{cb: live}
	if svc.allowCleanup(ctx, "sb-live") {
		t.Fatal("live unmarked cubebox was sent to cleanup")
	}
	left, err = store.readAll()
	if err != nil || len(left) != 0 {
		t.Fatalf("live sandbox stayed on the dirty list: %v %+v", err, left)
	}
}

// TestAllowCleanupKeepsPausedTombstoneRecord pins the I11 fix: a live,
// unmarked PAUSED tombstone — the shape a failed Create-from-pause rollback
// leaves behind — must NOT have its dirty record dropped. Dropping it would
// strand any host allocations the failed teardown left behind; reclaiming
// now would destroy the resumable pause package. allowCleanup keeps the
// record ("not yet") so every pass re-checks and the entry resolves once the
// tombstone's lifecycle ends (resumed → dropped as live, or deleted →
// proceeds and reclaims).
func TestAllowCleanupKeepsPausedTombstoneRecord(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := &local{config: &GCConfig{RootPath: dir}}
	if err := store.initDb(); err != nil {
		t.Fatal(err)
	}
	if err := store.saveSandBoxInfo(&sandBoxInfo{SandboxID: "sb-paused", Namespace: "default"}); err != nil {
		t.Fatal(err)
	}
	svc := &gcService{gc: store}

	// PAUSED + unmarked: keep the record, defer cleanup.
	paused := gcRow("sb-paused", cubeboxstore.Status{StartedAt: 1, PausedAt: time.Now().UnixNano()})
	svc.gc.cubeboxManger = gcBoxStub{cb: paused}
	if svc.allowCleanup(ctx, "sb-paused") {
		t.Fatal("a paused tombstone must not be sent to cleanup")
	}
	left, err := store.readAll()
	if err != nil || len(left) != 1 {
		t.Fatalf("paused tombstone dropped its dirty record (would strand teardown leftovers): %v %+v", err, left)
	}

	// The same tombstone, now user-delete marked: cleanup proceeds (the
	// record is reclaimed by the cleanup flow, not by allowCleanup).
	now := time.Now()
	paused.UserMarkDeletedTime = &now
	if !svc.allowCleanup(ctx, "sb-paused") {
		t.Fatal("a user-deleted paused tombstone must proceed to cleanup")
	}
}
