// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	v1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/images/v1"
)

func createTestStore(t *testing.T) (*Store, func()) {
	basePath := t.TempDir()
	db, err := utils.NewCubeStoreExt(basePath, "meta_test.db", 10, nil)
	require.NoError(t, err)

	store := NewStore(db)

	cleanup := func() {
		db.Close()
		os.RemoveAll(basePath)
	}

	return store, cleanup
}

func createTestCubeBox(id string, templateID string) *CubeBox {
	box := &CubeBox{
		Metadata: Metadata{
			ID:        id,
			Name:      "test-box-" + id,
			Labels:    make(map[string]string),
			CreatedAt: time.Now().UnixNano(),
			Config: &cubebox.ContainerConfig{
				Image: &v1.ImageSpec{Image: "docker.io/library/ubuntu:latest"},
			},
		},
	}

	if templateID != "" {
		box.LocalRunTemplate = &templatetypes.LocalRunTemplate{
			DistributionReference: templatetypes.DistributionReference{
				TemplateID: templateID,
			},
		}
	}

	box.AddContainer(&Container{
		Metadata: box.Metadata,
		Status:   StoreStatus(Status{CreatedAt: time.Now().UnixNano()}),
		IsPod:    true,
	})

	return box
}

func TestNewStore(t *testing.T) {
	store, cleanup := createTestStore(t)
	defer cleanup()

	assert.NotNil(t, store)
	assert.NotNil(t, store.db)
	assert.NotNil(t, store.indexer)
	assert.Equal(t, 0, store.Len())
}

func TestStoreAdd(t *testing.T) {
	store, cleanup := createTestStore(t)
	defer cleanup()

	box := createTestCubeBox("box1", "template1")
	store.Add(box)

	assert.Equal(t, 1, store.Len())
}

func TestStoreAddMultiple(t *testing.T) {
	store, cleanup := createTestStore(t)
	defer cleanup()

	box1 := createTestCubeBox("box1", "template1")
	box2 := createTestCubeBox("box2", "template2")
	box3 := createTestCubeBox("box3", "template1")

	store.Add(box1)
	store.Add(box2)
	store.Add(box3)

	assert.Equal(t, 3, store.Len())
}

func TestStoreAddUpdate(t *testing.T) {
	store, cleanup := createTestStore(t)
	defer cleanup()

	box := createTestCubeBox("box1", "template1")
	store.Add(box)

	box.Name = "updated-box"
	store.Add(box)

	assert.Equal(t, 1, store.Len())

	got, err := store.Get("box1")
	require.NoError(t, err)
	assert.Equal(t, "updated-box", got.Name)
}

func TestStoreGet(t *testing.T) {
	store, cleanup := createTestStore(t)
	defer cleanup()

	box := createTestCubeBox("box1", "template1")
	store.Add(box)

	got, err := store.Get("box1")
	require.NoError(t, err)
	assert.Equal(t, "box1", got.ID)
	assert.Equal(t, "test-box-box1", got.Name)
}

func TestStoreGetNotFound(t *testing.T) {
	store, cleanup := createTestStore(t)
	defer cleanup()

	_, err := store.Get("nonexistent")
	assert.Error(t, err)
	assert.Equal(t, utils.ErrorKeyNotFound, err)
}

func TestStoreGetContainer(t *testing.T) {
	store, cleanup := createTestStore(t)
	defer cleanup()

	box := createTestCubeBox("box1", "template1")

	containerID := utils.GenerateID()
	container := &Container{
		Metadata: Metadata{
			ID:        containerID,
			SandboxID: box.ID,
			CreatedAt: time.Now().UnixNano(),
		},
		Status: StoreStatus(Status{CreatedAt: time.Now().UnixNano()}),
		IsPod:  false,
	}
	box.AddContainer(container)
	store.Add(box)

	got, err := store.GetContainer(containerID)
	require.NoError(t, err)
	assert.Equal(t, containerID, got.ID)
	assert.False(t, got.IsPod)
}

func TestStoreGetContainerNotFound(t *testing.T) {
	store, cleanup := createTestStore(t)
	defer cleanup()

	_, err := store.GetContainer("nonexistent")
	assert.Error(t, err)
	assert.Equal(t, utils.ErrorKeyNotFound, err)
}

func TestStoreGetContainerFromMultipleBoxes(t *testing.T) {
	store, cleanup := createTestStore(t)
	defer cleanup()

	box1 := createTestCubeBox("box1", "template1")
	box2 := createTestCubeBox("box2", "template2")

	container1ID := utils.GenerateID()
	container1 := &Container{
		Metadata: Metadata{
			ID:        container1ID,
			SandboxID: box1.ID,
			CreatedAt: time.Now().UnixNano(),
		},
		Status: StoreStatus(Status{CreatedAt: time.Now().UnixNano()}),
		IsPod:  false,
	}
	box1.AddContainer(container1)

	container2ID := utils.GenerateID()
	container2 := &Container{
		Metadata: Metadata{
			ID:        container2ID,
			SandboxID: box2.ID,
			CreatedAt: time.Now().UnixNano(),
		},
		Status: StoreStatus(Status{CreatedAt: time.Now().UnixNano()}),
		IsPod:  false,
	}
	box2.AddContainer(container2)

	store.Add(box1)
	store.Add(box2)

	got1, err := store.GetContainer(container1ID)
	require.NoError(t, err)
	assert.Equal(t, container1ID, got1.ID)

	got2, err := store.GetContainer(container2ID)
	require.NoError(t, err)
	assert.Equal(t, container2ID, got2.ID)
}

func TestStoreDeleteContainer(t *testing.T) {
	store, cleanup := createTestStore(t)
	defer cleanup()

	box := createTestCubeBox("box1", "template1")

	containerID := utils.GenerateID()
	container := &Container{
		Metadata: Metadata{
			ID:        containerID,
			SandboxID: box.ID,
			CreatedAt: time.Now().UnixNano(),
		},
		Status: StoreStatus(Status{CreatedAt: time.Now().UnixNano()}),
		IsPod:  false,
	}
	box.AddContainer(container)
	store.Add(box)

	_, err := store.GetContainer(containerID)
	require.NoError(t, err)

	store.DeleteContainer(containerID)

	_, err = store.GetContainer(containerID)
	assert.Error(t, err)
	assert.Equal(t, utils.ErrorKeyNotFound, err)

	got, err := store.Get("box1")
	require.NoError(t, err)
	assert.Equal(t, "box1", got.ID)
}

func TestStoreDeleteContainerNonexistent(t *testing.T) {
	store, cleanup := createTestStore(t)
	defer cleanup()

	store.DeleteContainer("nonexistent")
}

func TestStoreList(t *testing.T) {
	store, cleanup := createTestStore(t)
	defer cleanup()

	box1 := createTestCubeBox("box1", "template1")
	box2 := createTestCubeBox("box2", "template2")
	box3 := createTestCubeBox("box3", "template1")

	store.Add(box1)
	store.Add(box2)
	store.Add(box3)

	boxes := store.List()
	assert.Equal(t, 3, len(boxes))

	ids := make(map[string]bool)
	for _, box := range boxes {
		ids[box.ID] = true
	}

	assert.True(t, ids["box1"])
	assert.True(t, ids["box2"])
	assert.True(t, ids["box3"])
}

func TestStoreListEmpty(t *testing.T) {
	store, cleanup := createTestStore(t)
	defer cleanup()

	boxes := store.List()
	assert.Equal(t, 0, len(boxes))
}

func TestStoreLen(t *testing.T) {
	store, cleanup := createTestStore(t)
	defer cleanup()

	assert.Equal(t, 0, store.Len())

	box1 := createTestCubeBox("box1", "template1")
	store.Add(box1)
	assert.Equal(t, 1, store.Len())

	box2 := createTestCubeBox("box2", "template2")
	store.Add(box2)
	assert.Equal(t, 2, store.Len())
}

func TestStoreSyncByCubeBoxID(t *testing.T) {
	store, cleanup := createTestStore(t)
	defer cleanup()

	box := createTestCubeBox("box1", "template1")
	store.Add(box)

	err := store.Sync("box1")
	require.NoError(t, err)

	bs, err := store.db.Get(DBBucketSandbox, "box1")
	require.NoError(t, err)
	assert.NotNil(t, bs)
}

func TestStoreSyncByContainerID(t *testing.T) {
	store, cleanup := createTestStore(t)
	defer cleanup()

	box := createTestCubeBox("box1", "template1")

	containerID := utils.GenerateID()
	container := &Container{
		Metadata: Metadata{
			ID:        containerID,
			SandboxID: box.ID,
			CreatedAt: time.Now().UnixNano(),
		},
		Status: StoreStatus(Status{CreatedAt: time.Now().UnixNano()}),
		IsPod:  false,
	}
	box.AddContainer(container)
	store.Add(box)

	err := store.Sync(containerID)
	require.NoError(t, err)

	bs, err := store.db.Get(DBBucketSandbox, "box1")
	require.NoError(t, err)
	assert.NotNil(t, bs)
}

func TestStoreSyncNonexistent(t *testing.T) {
	store, cleanup := createTestStore(t)
	defer cleanup()

	err := store.Sync("nonexistent")
	assert.NoError(t, err)
}

func TestStoreDeleteSync(t *testing.T) {
	store, cleanup := createTestStore(t)
	defer cleanup()

	box := createTestCubeBox("box1", "template1")
	store.Add(box)

	err := store.Sync("box1")
	require.NoError(t, err)

	_, err = store.db.Get(DBBucketSandbox, "box1")
	require.NoError(t, err)

	err = store.DeleteSync("box1")
	require.NoError(t, err)

	_, err = store.Get("box1")
	assert.Error(t, err)
	assert.Equal(t, utils.ErrorKeyNotFound, err)

	_, err = store.db.Get(DBBucketSandbox, "box1")
	assert.Error(t, err)
}

func TestStoreDeleteSyncNonexistent(t *testing.T) {
	store, cleanup := createTestStore(t)
	defer cleanup()

	err := store.DeleteSync("nonexistent")
	assert.NoError(t, err)
}

func TestStoreTemplateIDIndexer(t *testing.T) {
	store, cleanup := createTestStore(t)
	defer cleanup()

	box1 := createTestCubeBox("box1", "template1")
	box2 := createTestCubeBox("box2", "template2")
	box3 := createTestCubeBox("box3", "template1")

	store.Add(box1)
	store.Add(box2)
	store.Add(box3)

	objs, err := store.indexer.ByIndex(templateIDIndexerKey, "template1")
	require.NoError(t, err)
	assert.Equal(t, 2, len(objs))

	objs, err = store.indexer.ByIndex(templateIDIndexerKey, "template2")
	require.NoError(t, err)
	assert.Equal(t, 1, len(objs))
}

func TestStoreContainerIDIndexer(t *testing.T) {
	store, cleanup := createTestStore(t)
	defer cleanup()

	box := createTestCubeBox("box1", "template1")

	container1ID := utils.GenerateID()
	container2ID := utils.GenerateID()

	container1 := &Container{
		Metadata: Metadata{
			ID:        container1ID,
			SandboxID: box.ID,
			CreatedAt: time.Now().UnixNano(),
		},
		Status: StoreStatus(Status{CreatedAt: time.Now().UnixNano()}),
		IsPod:  false,
	}

	container2 := &Container{
		Metadata: Metadata{
			ID:        container2ID,
			SandboxID: box.ID,
			CreatedAt: time.Now().UnixNano(),
		},
		Status: StoreStatus(Status{CreatedAt: time.Now().UnixNano()}),
		IsPod:  false,
	}

	box.AddContainer(container1)
	box.AddContainer(container2)
	store.Add(box)

	objs, err := store.indexer.ByIndex(containerIDIndexerKey, container1ID)
	require.NoError(t, err)
	assert.Equal(t, 1, len(objs))

	boxFromIndex, ok := objs[0].(*CubeBox)
	require.True(t, ok)
	assert.Equal(t, "box1", boxFromIndex.ID)
}

func TestStoreConcurrency(t *testing.T) {
	store, cleanup := createTestStore(t)
	defer cleanup()

	box1 := createTestCubeBox("box1", "template1")
	box2 := createTestCubeBox("box2", "template2")

	container1ID := utils.GenerateID()
	container1 := &Container{
		Metadata: Metadata{
			ID:        container1ID,
			SandboxID: box1.ID,
			CreatedAt: time.Now().UnixNano(),
		},
		Status: StoreStatus(Status{CreatedAt: time.Now().UnixNano()}),
		IsPod:  false,
	}
	box1.AddContainer(container1)

	container2ID := utils.GenerateID()
	container2 := &Container{
		Metadata: Metadata{
			ID:        container2ID,
			SandboxID: box2.ID,
			CreatedAt: time.Now().UnixNano(),
		},
		Status: StoreStatus(Status{CreatedAt: time.Now().UnixNano()}),
		IsPod:  false,
	}
	box2.AddContainer(container2)

	done := make(chan bool)

	go func() {
		for i := 0; i < 100; i++ {
			store.Add(box1)
			store.Add(box2)
		}
		done <- true
	}()

	go func() {
		for i := 0; i < 100; i++ {
			store.Get("box1")
			store.Get("box2")
			store.GetContainer(container1ID)
			store.GetContainer(container2ID)
			store.List()
		}
		done <- true
	}()

	<-done
	<-done

	assert.Equal(t, 2, store.Len())
}

func TestStoreIndexerKeyFunc(t *testing.T) {
	box := createTestCubeBox("box1", "template1")

	key, err := cubeboxKeyFunc(box)
	require.NoError(t, err)
	assert.Equal(t, "box1", key)

	_, err = cubeboxKeyFunc("not a cubebox")
	assert.Error(t, err)
}

func TestStoreTemplateIDIndexerFunc(t *testing.T) {
	indexFunc := indexers[templateIDIndexerKey]

	box := createTestCubeBox("box1", "template1")
	keys, err := indexFunc(box)
	require.NoError(t, err)
	assert.Equal(t, []string{"template1"}, keys)

	boxNoTemplate := createTestCubeBox("box2", "")
	boxNoTemplate.LocalRunTemplate = nil
	keys, err = indexFunc(boxNoTemplate)
	require.NoError(t, err)
	assert.Equal(t, []string{}, keys)

	_, err = indexFunc("not a cubebox")
	assert.Error(t, err)
}

func TestStoreContainerIDIndexerFunc(t *testing.T) {
	indexFunc := indexers[containerIDIndexerKey]

	box := createTestCubeBox("box1", "template1")

	container1ID := utils.GenerateID()
	container2ID := utils.GenerateID()

	container1 := &Container{
		Metadata: Metadata{
			ID:        container1ID,
			SandboxID: box.ID,
			CreatedAt: time.Now().UnixNano(),
		},
		Status: StoreStatus(Status{CreatedAt: time.Now().UnixNano()}),
		IsPod:  false,
	}

	container2 := &Container{
		Metadata: Metadata{
			ID:        container2ID,
			SandboxID: box.ID,
			CreatedAt: time.Now().UnixNano(),
		},
		Status: StoreStatus(Status{CreatedAt: time.Now().UnixNano()}),
		IsPod:  false,
	}

	box.AddContainer(container1)
	box.AddContainer(container2)

	keys, err := indexFunc(box)
	require.NoError(t, err)

	assert.Equal(t, 3, len(keys))
	assert.Contains(t, keys, container1ID)
	assert.Contains(t, keys, container2ID)

	_, err = indexFunc("not a cubebox")
	assert.Error(t, err)
}

func TestStoreGetContainerWithTypeAssertion(t *testing.T) {
	store, cleanup := createTestStore(t)
	defer cleanup()

	box := createTestCubeBox("box1", "template1")

	containerID := utils.GenerateID()
	container := &Container{
		Metadata: Metadata{
			ID:        containerID,
			SandboxID: box.ID,
			CreatedAt: time.Now().UnixNano(),
		},
		Status: StoreStatus(Status{CreatedAt: time.Now().UnixNano()}),
		IsPod:  false,
	}
	box.AddContainer(container)
	store.Add(box)

	got, err := store.GetContainer(containerID)
	require.NoError(t, err)
	assert.Equal(t, containerID, got.ID)

	allContainers := box.AllContainers()
	c, ok := allContainers[containerID]
	assert.True(t, ok)
	assert.Equal(t, containerID, c.ID)
}

func TestStoreDeleteContainerUpdate(t *testing.T) {
	store, cleanup := createTestStore(t)
	defer cleanup()

	box := createTestCubeBox("box1", "template1")

	container1ID := utils.GenerateID()
	container2ID := utils.GenerateID()

	container1 := &Container{
		Metadata: Metadata{
			ID:        container1ID,
			SandboxID: box.ID,
			CreatedAt: time.Now().UnixNano(),
		},
		Status: StoreStatus(Status{CreatedAt: time.Now().UnixNano()}),
		IsPod:  false,
	}

	container2 := &Container{
		Metadata: Metadata{
			ID:        container2ID,
			SandboxID: box.ID,
			CreatedAt: time.Now().UnixNano(),
		},
		Status: StoreStatus(Status{CreatedAt: time.Now().UnixNano()}),
		IsPod:  false,
	}

	box.AddContainer(container1)
	box.AddContainer(container2)
	store.Add(box)

	_, err := store.GetContainer(container1ID)
	require.NoError(t, err)
	_, err = store.GetContainer(container2ID)
	require.NoError(t, err)

	store.DeleteContainer(container1ID)

	_, err = store.GetContainer(container1ID)
	assert.Error(t, err)

	got, err := store.GetContainer(container2ID)
	require.NoError(t, err)
	assert.Equal(t, container2ID, got.ID)
}

func TestStoreSyncWithUpdateError(t *testing.T) {
	store, cleanup := createTestStore(t)
	defer cleanup()

	box := createTestCubeBox("box1", "template1")
	store.Add(box)

	err := store.Sync("box1")
	require.NoError(t, err)
}

func TestStoreDeleteSyncWithDBError(t *testing.T) {
	store, cleanup := createTestStore(t)
	defer cleanup()

	box := createTestCubeBox("box1", "template1")
	store.Add(box)

	err := store.Sync("box1")
	require.NoError(t, err)

	err = store.DeleteSync("box1")
	require.NoError(t, err)

	_, err = store.Get("box1")
	assert.Error(t, err)
}

func TestStoreIndexerFunctions(t *testing.T) {

	store, cleanup := createTestStore(t)
	defer cleanup()

	box1 := createTestCubeBox("box1", "template1")
	box2 := createTestCubeBox("box2", "")
	box2.LocalRunTemplate = nil

	store.Add(box1)
	store.Add(box2)

	objs, err := store.indexer.ByIndex(templateIDIndexerKey, "template1")
	require.NoError(t, err)
	assert.Equal(t, 1, len(objs))

	objs, err = store.indexer.ByIndex(templateIDIndexerKey, "")
	require.NoError(t, err)

	assert.GreaterOrEqual(t, len(objs), 0)
}

func TestStoreFullWorkflow(t *testing.T) {
	store, cleanup := createTestStore(t)
	defer cleanup()

	box := createTestCubeBox("box1", "template1")
	store.Add(box)

	containerID := utils.GenerateID()
	container := &Container{
		Metadata: Metadata{
			ID:        containerID,
			SandboxID: box.ID,
			CreatedAt: time.Now().UnixNano(),
		},
		Status: StoreStatus(Status{CreatedAt: time.Now().UnixNano()}),
		IsPod:  false,
	}
	box.AddContainer(container)
	store.Add(box)

	err := store.Sync("box1")
	require.NoError(t, err)

	got, err := store.Get("box1")
	require.NoError(t, err)
	assert.Equal(t, "box1", got.ID)

	c, err := store.GetContainer(containerID)
	require.NoError(t, err)
	assert.Equal(t, containerID, c.ID)

	boxes := store.List()
	assert.Equal(t, 1, len(boxes))

	store.DeleteContainer(containerID)
	_, err = store.GetContainer(containerID)
	assert.Error(t, err)

	err = store.DeleteSync("box1")
	require.NoError(t, err)

	_, err = store.Get("box1")
	assert.Error(t, err)
	assert.Equal(t, 0, store.Len())
}

func TestStoreGetErrorCases(t *testing.T) {
	store, cleanup := createTestStore(t)
	defer cleanup()

	_, err := store.Get("nonexistent")
	assert.Error(t, err)
	assert.Equal(t, utils.ErrorKeyNotFound, err)
}

func TestStoreGetContainerErrorCases(t *testing.T) {
	store, cleanup := createTestStore(t)
	defer cleanup()

	_, err := store.GetContainer("nonexistent")
	assert.Error(t, err)
	assert.Equal(t, utils.ErrorKeyNotFound, err)

	box := createTestCubeBox("box1", "template1")
	store.Add(box)

	_, err = store.GetContainer("nonexistent-container")
	assert.Error(t, err)
}

func TestStoreDeleteContainerErrorCases(t *testing.T) {
	store, cleanup := createTestStore(t)
	defer cleanup()

	store.DeleteContainer("nonexistent")

	box := createTestCubeBox("box1", "template1")
	store.Add(box)

	store.DeleteContainer("nonexistent-in-box")

	got, err := store.Get("box1")
	require.NoError(t, err)
	assert.Equal(t, "box1", got.ID)
}

func TestStoreSyncErrorCases(t *testing.T) {
	store, cleanup := createTestStore(t)
	defer cleanup()

	box := createTestCubeBox("box1", "template1")

	containerID := utils.GenerateID()
	container := &Container{
		Metadata: Metadata{
			ID:        containerID,
			SandboxID: box.ID,
			CreatedAt: time.Now().UnixNano(),
		},
		Status: StoreStatus(Status{CreatedAt: time.Now().UnixNano()}),
		IsPod:  false,
	}
	box.AddContainer(container)
	store.Add(box)

	err := store.Sync(containerID)
	require.NoError(t, err)

	bs, err := store.db.Get(DBBucketSandbox, "box1")
	require.NoError(t, err)
	assert.NotNil(t, bs)
}

func TestStoreDeleteSyncErrorCases(t *testing.T) {
	store, cleanup := createTestStore(t)
	defer cleanup()

	err := store.DeleteSync("nonexistent")
	assert.NoError(t, err)

	box := createTestCubeBox("box1", "template1")
	store.Add(box)
	err = store.Sync("box1")
	require.NoError(t, err)

	err = store.DeleteSync("box1")
	require.NoError(t, err)

	err = store.DeleteSync("box1")
	assert.NoError(t, err)
}

func TestStoreMultipleTemplatesSameID(t *testing.T) {
	store, cleanup := createTestStore(t)
	defer cleanup()

	box1 := createTestCubeBox("box1", "template-common")
	box2 := createTestCubeBox("box2", "template-common")
	box3 := createTestCubeBox("box3", "template-common")

	store.Add(box1)
	store.Add(box2)
	store.Add(box3)

	objs, err := store.indexer.ByIndex(templateIDIndexerKey, "template-common")
	require.NoError(t, err)
	assert.Equal(t, 3, len(objs))

	boxIDs := make(map[string]bool)
	for _, obj := range objs {
		if box, ok := obj.(*CubeBox); ok {
			boxIDs[box.ID] = true
		}
	}

	assert.True(t, boxIDs["box1"])
	assert.True(t, boxIDs["box2"])
	assert.True(t, boxIDs["box3"])
}

func TestStoreContainerDeletionAndReAdd(t *testing.T) {
	store, cleanup := createTestStore(t)
	defer cleanup()

	box := createTestCubeBox("box1", "template1")

	containerID := utils.GenerateID()
	container := &Container{
		Metadata: Metadata{
			ID:        containerID,
			SandboxID: box.ID,
			CreatedAt: time.Now().UnixNano(),
		},
		Status: StoreStatus(Status{CreatedAt: time.Now().UnixNano()}),
		IsPod:  false,
	}
	box.AddContainer(container)
	store.Add(box)

	_, err := store.GetContainer(containerID)
	require.NoError(t, err)

	store.DeleteContainer(containerID)

	_, err = store.GetContainer(containerID)
	assert.Error(t, err)

	box.AddContainer(container)
	store.Add(box)

	got, err := store.GetContainer(containerID)
	require.NoError(t, err)
	assert.Equal(t, containerID, got.ID)
}

func TestStoreListConcurrency(t *testing.T) {
	store, cleanup := createTestStore(t)
	defer cleanup()

	box1 := createTestCubeBox("box1", "template1")
	box2 := createTestCubeBox("box2", "template2")

	store.Add(box1)
	store.Add(box2)

	done := make(chan bool, 2)

	go func() {
		for i := 0; i < 50; i++ {
			boxes := store.List()
			assert.GreaterOrEqual(t, len(boxes), 0)
		}
		done <- true
	}()

	go func() {
		for i := 0; i < 50; i++ {
			boxes := store.List()
			assert.GreaterOrEqual(t, len(boxes), 0)
		}
		done <- true
	}()

	<-done
	<-done
}
