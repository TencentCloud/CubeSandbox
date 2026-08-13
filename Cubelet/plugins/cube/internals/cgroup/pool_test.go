// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cgroup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/containerd/cgroups/v3"
	"github.com/containerd/cgroups/v3/cgroup1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cgroup/handle"
)

var testLock sync.Mutex

func requireWritableCgroupFilesystem(t *testing.T) {
	t.Helper()

	if cgroups.Mode() == cgroups.Unified {
		probeWritableCgroupDirectory(t, handle.RootMountPoint)
		return
	}

	subsystems, err := cgroup1.Default()
	if err != nil {
		t.Skipf("requires available cgroup v1 controllers: %v", err)
	}

	required := map[cgroup1.Name]bool{
		cgroup1.Cpu:    false,
		cgroup1.Memory: false,
		cgroup1.Cpuset: false,
	}
	for _, subsystem := range subsystems {
		if _, ok := required[subsystem.Name()]; !ok {
			continue
		}
		pather, ok := subsystem.(interface{ Path(string) string })
		if !ok {
			t.Skipf("cgroup v1 controller %q does not expose its mount path", subsystem.Name())
		}
		probeWritableCgroupDirectory(t, pather.Path(""))
		required[subsystem.Name()] = true
	}
	for controller, found := range required {
		if !found {
			t.Skipf("requires cgroup v1 controller %q", controller)
		}
	}
}

func probeWritableCgroupDirectory(t *testing.T, root string) {
	t.Helper()

	probe, err := os.MkdirTemp(root, ".cubelet-cgroup-test-")
	if err != nil {
		t.Skipf("requires writable cgroup directory %q: %v", root, err)
	}
	t.Cleanup(func() {
		_ = os.Remove(probe)
	})
	if err := os.Remove(probe); err != nil {
		t.Fatalf("remove writable cgroup probe %q: %v", probe, err)
	}
}

func TestPoolBasicOP(t *testing.T) {
	utils.SkipCI(t)
	requireWritableCgroupFilesystem(t)

	ctx := context.Background()
	testLock.Lock()
	defer testLock.Unlock()

	testDb := filepath.Join(t.TempDir(), "db")

	var (
		cg  *uint32
		err error
	)

	db, err := utils.NewCubeStoreExt(testDb, "meta.db", 10, nil)
	require.NoErrorf(t, err, "create db")

	p := &cgPool{
		initialSize:  10,
		poolV1Handle: getDefaultCgroupHandle(1),
		poolV2Handle: getDefaultCgroupHandle(2),
		db:           db,
	}
	err = p.init()
	require.NoErrorf(t, err, "init pool")

	cgList, err := p.poolV1Handle.List()
	assert.NoError(t, err)
	assert.LessOrEqual(t, 10, len(cgList))

	cg, err = p.Get(ctx, "test1", false, 0)
	require.NoError(t, err)
	assert.Equal(t, uint32(0), *cg)
	assert.ElementsMatch(t, []uint32{0}, p.All())

	cg, err = p.Get(ctx, "test2", false, 0)
	require.NoError(t, err)
	assert.Equal(t, uint32(1), *cg)
	assert.ElementsMatch(t, []uint32{0, 1}, p.All())

	p.Put(context.TODO(), uint32(1))
	assert.ElementsMatch(t, []uint32{0}, p.All())

	cg, err = p.Get(ctx, "test3", false, 0)
	require.NoError(t, err)
	assert.Equal(t, uint32(2), *cg)
	assert.ElementsMatch(t, []uint32{0, 2}, p.All())

	err = db.Set(bucket, "test1", []byte("0"))
	require.NoError(t, err)
	err = db.Set(bucket, "test3", []byte("2"))
	require.NoError(t, err)

	p.init()
	assert.Equal(t, 2, len(p.All()))

	errs := p.Tidy()
	if len(errs) > 0 {
		err = fmt.Errorf("Tidy() returned errors: %v", errs)
	}
	assert.NoError(t, err)
	assert.ElementsMatch(t, []uint32{0, 2}, p.All())

	cgList, err = p.poolV1Handle.List()
	assert.NoError(t, err)
	assert.LessOrEqual(t, 10, len(cgList))

	p.dirtySet[0] = struct{}{}

	errs = p.Tidy()
	if len(errs) > 0 {
		err = fmt.Errorf("Tidy() returned errors: %v", errs)
	}
	assert.NoError(t, err)
	assert.ElementsMatch(t, []uint32{2}, p.All())

	cgList, err = p.poolV1Handle.List()
	assert.NoError(t, err)
	assert.LessOrEqual(t, 10, len(cgList))
}

func TestPoolTidy(t *testing.T) {
	utils.SkipCI(t)
	requireWritableCgroupFilesystem(t)

	testLock.Lock()
	defer testLock.Unlock()

	testDb := filepath.Join(t.TempDir(), "db")

	db, err := utils.NewCubeStoreExt(testDb, "meta.db", 10, nil)
	require.NoErrorf(t, err, "create db")

	p := &cgPool{
		initialSize:  10,
		poolV1Handle: getDefaultCgroupHandle(1),
		poolV2Handle: getDefaultCgroupHandle(2),
		db:           db,
	}
	err = p.init()
	require.NoErrorf(t, err, "init pool")

	cgList, err := p.poolV1Handle.List()
	assert.NoError(t, err)
	assert.LessOrEqual(t, 11, len(cgList))

	err = p.poolV1Handle.Create(context.TODO(), MakeCgroupPoolV1PathByString("somecgunknown"))
	assert.NoError(t, err)

	p = &cgPool{
		initialSize:  0,
		poolV1Handle: getDefaultCgroupHandle(1),
		poolV2Handle: getDefaultCgroupHandle(2),
		db:           db,
	}
	err = p.init()
	require.NoErrorf(t, err, "init pool")
	errs := p.Tidy()
	if len(errs) > 0 {
		err = fmt.Errorf("Tidy() returned errors: %v", errs)
	}
	assert.NoError(t, err)

	cgList, err = p.poolV1Handle.List()
	assert.NoError(t, err)
	assert.Equal(t, 1, len(cgList))
}

func TestPoolExpand(t *testing.T) {
	utils.SkipCI(t)
	requireWritableCgroupFilesystem(t)

	ctx := context.Background()

	testLock.Lock()
	defer testLock.Unlock()

	var (
		cg  *uint32
		err error
	)

	testDb := filepath.Join(t.TempDir(), "db")

	db, err := utils.NewCubeStoreExt(testDb, "meta.db", 10, nil)
	require.NoErrorf(t, err, "create db")

	p := &cgPool{
		initialSize:  1,
		poolV1Handle: getDefaultCgroupHandle(1),
		poolV2Handle: getDefaultCgroupHandle(2),
		db:           db,
	}
	err = p.init()
	require.NoErrorf(t, err, "init pool")

	cgList, err := p.poolV1Handle.List()
	originSize := len(cgList)
	assert.NoError(t, err)
	assert.LessOrEqual(t, 1, len(cgList))

	cg, err = p.Get(ctx, "test1", false, 0)
	require.NoError(t, err)
	assert.Equal(t, uint32(0), *cg)
	assert.ElementsMatch(t, []uint32{0}, p.All())

	cg, err = p.Get(ctx, "test2", false, 0)
	require.NoError(t, err)
	assert.Equal(t, uint32(1), *cg)
	assert.ElementsMatch(t, []uint32{0, 1}, p.All())

	cg, err = p.Get(ctx, "test3", false, 0)
	require.NoError(t, err)
	assert.Equal(t, uint32(2), *cg)
	assert.ElementsMatch(t, []uint32{0, 1, 2}, p.All())

	cgList, err = p.poolV1Handle.List()
	assert.NoError(t, err)

	assert.Equal(t, originSize+1, len(cgList))
}
