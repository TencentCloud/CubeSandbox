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
