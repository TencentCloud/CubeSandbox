// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"math/rand"
	"os"
	"testing"
	"time"

	"encoding/json"
	containerd "github.com/containerd/containerd/v2/client"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	v1 "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/images/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

type OldCubeBox struct {
	Metadata

	Status    *StatusStorage
	Namespace string

	IP           string
	PortMappings []*cubebox.PortMapping

	Container     containerd.Container `json:"-"`
	Containers    map[string]*Container
	ContainersMap *ContainersMap

	ExitCh <-chan containerd.ExitStatus `json:"-"`
}

func (cb *OldCubeBox) AddContainer(ctr *Container) {
	if cb.ContainersMap == nil {
		cb.ContainersMap = &ContainersMap{}
	}
	cb.ContainersMap.AddContainer(ctr)
}

func (cb *OldCubeBox) DeleteContainer(id string) {
	if cb.Containers != nil {
		delete(cb.Containers, id)
	}
	if cb.ContainersMap == nil {
		return
	}
	cb.ContainersMap.DeleteContainer(id)
}

func TestCubeBoxOldFromNew(t *testing.T) {
	info := CubeBox{
		Metadata: Metadata{
			ID:        utils.GenerateID(),
			Labels:    make(map[string]string),
			CreatedAt: time.Now().UnixNano(),
		},
	}

	info.AddContainer(&Container{
		Metadata: info.Metadata,
		Status:   StoreStatus(Status{CreatedAt: time.Now().UnixNano(), Pid: rand.Uint32(), StartedAt: time.Now().UnixNano()}),
		IsPod:    true,
	})

	ci := &Container{
		Metadata: Metadata{
			ID:        utils.GenerateID(),
			SandboxID: info.ID,
			CreatedAt: time.Now().UnixNano(),
			Config: &cubebox.ContainerConfig{
				Image: &v1.ImageSpec{Image: "docker.io/library/ubuntu:latest"},
			},
		},
		Status: StoreStatus(Status{CreatedAt: time.Now().UnixNano(), Pid: rand.Uint32(), StartedAt: time.Now().UnixNano()}),
		IsPod:  false,
	}
	info.AddContainer(ci)
	assert.Equal(t, 1, len(info.All()))
	ctr, err := info.Get(ci.ID)
	assert.NoError(t, err)
	assert.Equal(t, ci.ID, ctr.ID)
	assert.Equal(t, cubebox.ContainerState_CONTAINER_RUNNING, ctr.Status.Status.State())

	bs, err := json.Marshal(&info)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var cb CubeBox
	if err := json.Unmarshal(bs, &cb); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	assert.Equal(t, info.ID, cb.ID)
	assert.Equal(t, 1, len(cb.All()))
	assert.Equal(t, cubebox.ContainerState_CONTAINER_RUNNING, cb.GetStatus().Status.State())

	cntr, err := cb.Get(ci.ID)
	assert.NoError(t, err)
	assert.Equal(t, ci.ID, cntr.ID)
	assert.Equal(t, cubebox.ContainerState_CONTAINER_RUNNING, cntr.Status.Status.State())

	var oldcb OldCubeBox
	if err := json.Unmarshal(bs, &oldcb); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	assert.Equal(t, info.ID, cb.ID)
	assert.Equal(t, 2, len(oldcb.ContainersMap.ContainerMap))
	assert.Equal(t, cubebox.ContainerState_CONTAINER_RUNNING, cb.GetStatus().Status.State())

	cntr = oldcb.ContainersMap.ContainerMap[ci.ID]
	assert.Equal(t, ci.ID, cntr.ID)
	assert.Equal(t, cubebox.ContainerState_CONTAINER_RUNNING, cntr.Status.Status.State())
}

func TestCubeBoxNewFromOld(t *testing.T) {
	oldInfo := OldCubeBox{
		Metadata: Metadata{
			ID:        utils.GenerateID(),
			Labels:    make(map[string]string),
			CreatedAt: time.Now().UnixNano(),
		},
		Status: StoreStatus(Status{CreatedAt: time.Now().UnixNano(), Pid: rand.Uint32(), StartedAt: time.Now().UnixNano()}),
	}
	oldInfo.AddContainer(&Container{
		Metadata: oldInfo.Metadata,
		Status:   StoreStatus(Status{CreatedAt: time.Now().UnixNano(), Pid: rand.Uint32(), StartedAt: time.Now().UnixNano()}),
		IsPod:    true,
	})
	ci := &Container{
		Metadata: Metadata{
			ID:        utils.GenerateID(),
			SandboxID: oldInfo.ID,
			CreatedAt: time.Now().UnixNano(),
			Config: &cubebox.ContainerConfig{
				Image: &v1.ImageSpec{Image: "docker.io/library/ubuntu:latest"},
			},
		},
		Status: StoreStatus(Status{CreatedAt: time.Now().UnixNano(), Pid: rand.Uint32(), StartedAt: time.Now().UnixNano()}),
		IsPod:  false,
	}
	oldInfo.AddContainer(ci)
	assert.Equal(t, 2, len(oldInfo.ContainersMap.ContainerMap))
	cntr := oldInfo.ContainersMap.ContainerMap[ci.ID]
	assert.Equal(t, ci.ID, cntr.ID)
	assert.Equal(t, cubebox.ContainerState_CONTAINER_RUNNING, cntr.Status.Status.State())

	oldbs, err := json.Marshal(&oldInfo)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var oldcb OldCubeBox
	if err := json.Unmarshal(oldbs, &oldcb); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	assert.Equal(t, 2, len(oldcb.ContainersMap.ContainerMap))
	cntr = oldcb.ContainersMap.ContainerMap[ci.ID]
	assert.Equal(t, ci.ID, cntr.ID)
	assert.Equal(t, cubebox.ContainerState_CONTAINER_RUNNING, cntr.Status.Status.State())

	var newcb CubeBox
	if err := json.Unmarshal(oldbs, &newcb); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	newcb.Transmition()

	assert.Equal(t, oldInfo.ID, newcb.ID)
	assert.Equal(t, 1, len(newcb.All()))
	assert.Equal(t, cubebox.ContainerState_CONTAINER_RUNNING, newcb.GetStatus().Status.State())

	cntr, err = newcb.Get(ci.ID)
	assert.NoError(t, err)
	assert.Equal(t, ci.ID, cntr.ID)
	assert.Equal(t, cubebox.ContainerState_CONTAINER_RUNNING, cntr.Status.Status.State())

}

func TestCubeBoxEncodeDecode(t *testing.T) {
	info := CubeBox{
		Metadata: Metadata{
			ID:        utils.GenerateID(),
			Labels:    make(map[string]string),
			CreatedAt: time.Now().UnixNano(),
		},
	}
	info.AddContainer(&Container{
		Metadata: info.Metadata,
		Status:   StoreStatus(Status{CreatedAt: time.Now().UnixNano(), Pid: rand.Uint32(), StartedAt: time.Now().UnixNano()}),
		IsPod:    true,
	})

	ci := &Container{
		Metadata: Metadata{
			ID:        utils.GenerateID(),
			SandboxID: info.ID,
			CreatedAt: time.Now().UnixNano(),
			Config: &cubebox.ContainerConfig{
				Image: &v1.ImageSpec{Image: "docker.io/library/ubuntu:latest"},
			},
		},
		Status: StoreStatus(Status{CreatedAt: time.Now().UnixNano(), Pid: rand.Uint32(), StartedAt: time.Now().UnixNano()}),
		IsPod:  false,
	}
	info.AddContainer(ci)
	assert.Equal(t, 1, len(info.All()))
	ctr, err := info.Get(ci.ID)
	assert.NoError(t, err)
	assert.Equal(t, ci.ID, ctr.ID)
	assert.Equal(t, cubebox.ContainerState_CONTAINER_RUNNING, ctr.Status.Status.State())

	bs, err := json.Marshal(&info)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var cb CubeBox
	if err := json.Unmarshal(bs, &cb); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	assert.Equal(t, info.ID, cb.ID)
	assert.Equal(t, cubebox.ContainerState_CONTAINER_RUNNING, cb.GetStatus().Status.State())

	cntr, err := cb.Get(ci.ID)
	assert.NoError(t, err)
	assert.Equal(t, ci.ID, cntr.ID)
	assert.Equal(t, cubebox.ContainerState_CONTAINER_RUNNING, cntr.Status.Status.State())
}

func TestAddContainer(t *testing.T) {
	basePath := t.TempDir()
	db, err := utils.NewCubeStoreExt(basePath, "meta.db", 10, nil)
	if err != nil {
		assert.Nil(t, err)
		t.FailNow()
	}

	defer func() {
		_ = db.Close()
		_ = os.RemoveAll(basePath)
	}()

	cubestore := NewStore(db)

	info := &CubeBox{
		Metadata: Metadata{
			ID:        utils.GenerateID(),
			Labels:    make(map[string]string),
			CreatedAt: time.Now().UnixNano(),
			Config: &cubebox.ContainerConfig{
				Image: &v1.ImageSpec{Image: "docker.io/library/ubuntu:latest"},
			},
		},
	}
	info.AddContainer(&Container{
		Metadata: info.Metadata,
		Status:   StoreStatus(Status{CreatedAt: time.Now().UnixNano()}),
		IsPod:    true,
	})
	cubestore.Add(info)
	assert.Equal(t, 1, cubestore.Len())

	got, err := cubestore.Get(info.ID)
	assert.Nil(t, err)
	assert.Equal(t, info.ID, got.ID)
	assert.Equal(t, info.GetStatus().Get().State(), got.GetStatus().Get().State())
	assert.Equal(t, cubebox.ContainerState_CONTAINER_CREATED, got.GetStatus().Status.State())

	ci := &Container{
		Metadata: Metadata{
			ID:        utils.GenerateID(),
			SandboxID: info.ID,
			CreatedAt: time.Now().UnixNano(),
		},
		Status: StoreStatus(Status{CreatedAt: time.Now().UnixNano()}),
		IsPod:  false,
	}
	info.AddContainer(ci)
	cubestore.Add(info)
	assert.Equal(t, 1, cubestore.Len())
	assert.Equal(t, 1, len(info.All()))

	ctr, err := cubestore.GetContainer(ci.ID)
	assert.Nil(t, err)
	assert.Equal(t, ci.ID, ctr.ID)
	assert.False(t, ctr.IsPod)
	assert.Equal(t, cubebox.ContainerState_CONTAINER_CREATED, ctr.Status.Status.State())

	info.GetStatus().Update(func(status Status) (Status, error) {
		status.StartedAt = time.Now().UnixNano()
		return status, nil
	})
	assert.Equal(t, 1, cubestore.Len())

	ctr.Status.Update(func(status Status) (Status, error) {
		status.Pid = rand.Uint32()
		status.StartedAt = time.Now().UnixNano()
		return status, nil
	})
	cubestore.Add(info)
	assert.Equal(t, 1, cubestore.Len())
	got, err = cubestore.Get(info.ID)
	assert.Nil(t, err)
	assert.Equal(t, info.ID, got.ID)
	assert.True(t, got.GetStatus().Status.State() == cubebox.ContainerState_CONTAINER_RUNNING)

	ctr, err = cubestore.GetContainer(ci.ID)
	if err != nil {
		t.Fatalf("GetContainer error: %v", err)
	}
	assert.Equal(t, ctr.ID, ci.ID)
	assert.True(t, ctr.Status.Status.State() == cubebox.ContainerState_CONTAINER_RUNNING)

	bs, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	if err := db.Set(DBBucketSandbox, info.ID, bs); err != nil {
		t.Fatalf("Set error: %v", err)
	}

	sandboxBytes, err := db.Get(DBBucketSandbox, info.ID)
	if err != nil {
		t.Fatalf("Get error: %v", err)
	}
	var cb CubeBox
	if err := json.Unmarshal(sandboxBytes, &cb); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	assert.Equal(t, info.ID, cb.ID)
	assert.True(t, cb.GetStatus().Status.State() == cubebox.ContainerState_CONTAINER_RUNNING)
	assert.Equal(t, 1, len(cb.All()))

	ctr, ok := cb.All()[ci.ID]
	if !ok {
		t.Fatalf("Container not found")
	}
	assert.Equal(t, ctr.ID, ci.ID)
	assert.True(t, ctr.Status.Status.State() == cubebox.ContainerState_CONTAINER_RUNNING)

	cubeboxInStore := cubestore.List()
	assert.Equal(t, 1, len(cubeboxInStore))
	for _, box := range cubeboxInStore {
		if box.GetStatus().Get().Removing {
			continue
		}
		if box.ID != info.ID {
			t.Fatalf("Sandbox not found")
		}
		for _, c := range box.All() {
			if c.ID != ci.ID {
				t.Fatalf("Container not found")
			}

		}
	}
}

func listCubeStoreJob(ctx context.Context, cubestore *Store) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		for _, box := range cubestore.List() {
			if box.GetStatus() == nil {
				continue
			}
			if box.GetStatus().Get().Removing {
				continue
			}
			for range box.All() {
			}
		}
	}
}

func creatingCubeboxJob(ctx context.Context, cubestore *Store) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		info := &CubeBox{
			Metadata: Metadata{
				ID:        utils.GenerateID(),
				Labels:    make(map[string]string),
				CreatedAt: time.Now().UnixNano(),
			},
		}

		sandbox := &Container{
			Metadata: Metadata{
				ID:        utils.GenerateID(),
				SandboxID: info.ID,
				CreatedAt: time.Now().UnixNano(),
			},
			Status: StoreStatus(Status{CreatedAt: time.Now().UnixNano(), Pid: rand.Uint32(), StartedAt: time.Now().UnixNano()}),
			IsPod:  true,
		}
		info.AddContainer(sandbox)
		cubestore.Add(info)
		cubestore.Sync(info.ID)
		time.Sleep(10 * time.Millisecond)
		ci := &Container{
			Metadata: Metadata{
				ID:        utils.GenerateID(),
				SandboxID: info.ID,
				CreatedAt: time.Now().UnixNano(),
			},
			Status: StoreStatus(Status{CreatedAt: time.Now().UnixNano(), Pid: rand.Uint32(), StartedAt: time.Now().UnixNano()}),
			IsPod:  false,
		}
		info.AddContainer(ci)
		cubestore.Add(info)
		cubestore.Sync(info.ID)
	}
}

func testMultiConcurrentOpContainer(jobNum int, basePath string) {
	db, err := utils.NewCubeStoreExt(basePath, "meta.db", 10, nil)
	if err != nil {
		return
	}

	defer func() {
		_ = db.Close()
		_ = os.RemoveAll(basePath)
	}()

	cubestore := NewStore(db)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; i < jobNum; i++ {
		go listCubeStoreJob(ctx, cubestore)
	}
	for i := 0; i < jobNum; i++ {
		go creatingCubeboxJob(ctx, cubestore)
	}
	select {
	case <-time.After(10 * time.Second):
		cancel()
	case <-ctx.Done():
	}
	<-ctx.Done()
}
func TestMultiConcurrentOpContainer(t *testing.T) {
	testMultiConcurrentOpContainer(10, t.TempDir())
}

func BenchmarkConcurrentOpContainer(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		testMultiConcurrentOpContainer(10, b.TempDir())
	}
}
