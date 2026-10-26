// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package runtemplate

import (
	"context"
	"testing"

	"github.com/containerd/containerd/v2/pkg/namespaces"

	imagestore "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/store/image"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cdp"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/membolt"
)

func TestImageDeleteHook(t *testing.T) {
	store := createMockStore(t)
	manager := &localCubeRunTemplateManager{
		store: store,
	}
	hook := &imageDeleteHook{manager}

	t.Run("Name", func(t *testing.T) {
		name := hook.Name()
		expected := "distribution-image-delete-hook"
		if name != expected {
			t.Errorf("Name() = %v, want %v", name, expected)
		}
	})

	t.Run("PostDelete does nothing", func(t *testing.T) {
		ctx := namespaces.WithNamespace(context.Background(), "default")
		err := hook.PostDelete(ctx, &cdp.DeleteOption{
			ID:           "test-image",
			ResourceType: cdp.ResourceDeleteProtectionTypeImage,
		})
		if err != nil {
			t.Errorf("PostDelete() unexpected error: %v", err)
		}
	})

	t.Run("PreDelete - no namespace", func(t *testing.T) {
		ctx := context.Background()
		err := hook.PreDelete(ctx, &cdp.DeleteOption{
			ID:           "test-image",
			ResourceType: cdp.ResourceDeleteProtectionTypeImage,
		})
		if err == nil {
			t.Error("PreDelete() expected error for missing namespace, got nil")
		}
	})

	t.Run("PreDelete - image not in use", func(t *testing.T) {
		ctx := namespaces.WithNamespace(context.Background(), "default")
		err := hook.PreDelete(ctx, &cdp.DeleteOption{
			ID:           "unused-image",
			ResourceType: cdp.ResourceDeleteProtectionTypeImage,
		})
		if err != nil {
			t.Errorf("PreDelete() unexpected error for unused image: %v", err)
		}
	})

	t.Run("PreDelete - image in use", func(t *testing.T) {
		ctx := namespaces.WithNamespace(context.Background(), "test-ns")

		template := &templatetypes.LocalRunTemplate{
			DistributionReference: templatetypes.DistributionReference{
				Namespace:          "test-ns",
				Name:               "test-template",
				TemplateID:         "template-123",
				DistributionTaskID: "task-123",
			},
			Images: []templatetypes.LocalDistributionImage{
				{
					Image: imagestore.Image{
						ID: "test-image",
					},
				},
			},
		}
		err := store.Update(template)
		if err != nil {
			t.Fatalf("Failed to add template: %v", err)
		}

		err = hook.PreDelete(ctx, &cdp.DeleteOption{
			ID:           "test-image",
			ResourceType: cdp.ResourceDeleteProtectionTypeImage,
		})
		if err == nil {
			t.Error("PreDelete() expected error for image in use, got nil")
		}
	})
}

func TestBaseBlockDeleteHook(t *testing.T) {
	store := createMockStore(t)
	manager := &localCubeRunTemplateManager{
		store: store,
	}
	hook := &baseBlockDeleteHook{manager}

	t.Run("Name", func(t *testing.T) {
		name := hook.Name()
		expected := "distribution-base-block-delete-hook"
		if name != expected {
			t.Errorf("Name() = %v, want %v", name, expected)
		}
	})

	t.Run("PostDelete does nothing", func(t *testing.T) {
		ctx := namespaces.WithNamespace(context.Background(), "default")
		err := hook.PostDelete(ctx, &cdp.DeleteOption{
			ID:           "test-volume",
			ResourceType: cdp.ResourceDeleteProtectionTypeStorageBaseBlock,
		})
		if err != nil {
			t.Errorf("PostDelete() unexpected error: %v", err)
		}
	})

	t.Run("PreDelete - no namespace", func(t *testing.T) {
		ctx := context.Background()
		err := hook.PreDelete(ctx, &cdp.DeleteOption{
			ID:           "test-volume",
			ResourceType: cdp.ResourceDeleteProtectionTypeStorageBaseBlock,
		})
		if err == nil {
			t.Error("PreDelete() expected error for missing namespace, got nil")
		}
	})

	t.Run("PreDelete - volume not in use", func(t *testing.T) {
		ctx := namespaces.WithNamespace(context.Background(), "default")
		err := hook.PreDelete(ctx, &cdp.DeleteOption{
			ID:           "unused-volume",
			ResourceType: cdp.ResourceDeleteProtectionTypeStorageBaseBlock,
		})
		if err != nil {
			t.Errorf("PreDelete() unexpected error for unused volume: %v", err)
		}
	})

	t.Run("PreDelete - volume in use", func(t *testing.T) {
		ctx := namespaces.WithNamespace(context.Background(), "test-ns")

		template := &templatetypes.LocalRunTemplate{
			DistributionReference: templatetypes.DistributionReference{
				Namespace:          "test-ns",
				Name:               "test-template",
				TemplateID:         "template-456",
				DistributionTaskID: "task-456",
			},
			Volumes: map[string]templatetypes.LocalBaseVolume{
				"vol-1": {
					VolumeID: "test-volume",
					Volume: templatetypes.VolumeSource{
						BaseBlockSource: templatetypes.BaseBlockVolumeSource{
							ID: "test-volume",
						},
					},
				},
			},
		}
		err := store.Update(template)
		if err != nil {
			t.Fatalf("Failed to add template: %v", err)
		}

		err = hook.PreDelete(ctx, &cdp.DeleteOption{
			ID:           "test-volume",
			ResourceType: cdp.ResourceDeleteProtectionTypeStorageBaseBlock,
		})
		if err == nil {
			t.Error("PreDelete() expected error for volume in use, got nil")
		}
	})
}

func TestSnapshotDeleteHook(t *testing.T) {
	store := createMockStore(t)
	manager := &localCubeRunTemplateManager{
		store: store,
	}
	hook := &snapshotDeleteHook{manager}

	t.Run("Name", func(t *testing.T) {
		name := hook.Name()
		expected := "distribution-snapshot-delete-hook"
		if name != expected {
			t.Errorf("Name() = %v, want %v", name, expected)
		}
	})

	t.Run("PostDelete does nothing", func(t *testing.T) {
		ctx := context.Background()
		err := hook.PostDelete(ctx, &cdp.DeleteOption{
			ID:           "test-snapshot",
			ResourceType: cdp.ResourceTypeVmSnapshot,
		})
		if err != nil {
			t.Errorf("PostDelete() unexpected error: %v", err)
		}
	})

	t.Run("PreDelete - snapshot not in use", func(t *testing.T) {
		ctx := context.Background()
		err := hook.PreDelete(ctx, &cdp.DeleteOption{
			ID:           "unused-snapshot",
			ResourceType: cdp.ResourceTypeVmSnapshot,
		})
		if err != nil {
			t.Errorf("PreDelete() unexpected error for unused snapshot: %v", err)
		}
	})

	t.Run("PreDelete - snapshot in use", func(t *testing.T) {
		ctx := context.Background()

		template := &templatetypes.LocalRunTemplate{
			DistributionReference: templatetypes.DistributionReference{
				Namespace:          "default",
				Name:               "test-template",
				TemplateID:         "template-789",
				DistributionTaskID: "task-789",
			},
			Snapshot: templatetypes.LocalSnapshot{
				Snapshot: templatetypes.Snapshot{
					ID: "test-snapshot",
				},
			},
		}
		err := store.Update(template)
		if err != nil {
			t.Fatalf("Failed to add template: %v", err)
		}

		err = hook.PreDelete(ctx, &cdp.DeleteOption{
			ID:           "test-snapshot",
			ResourceType: cdp.ResourceTypeVmSnapshot,
		})
		if err == nil {
			t.Error("PreDelete() expected error for snapshot in use, got nil")
		}
	})
}

func createMockStore(t *testing.T) *membolt.BoltCacheStore[*templatetypes.LocalRunTemplate] {
	db := &mockMetadataDB{
		data: make(map[string][]byte),
	}
	store, _ := membolt.NewBoltCacheStore(db, taskIDKeyFunc, indexer, &templatetypes.LocalRunTemplate{})
	return store
}

type mockMetadataDB struct {
	data map[string][]byte
}

func (m *mockMetadataDB) Get(bucket, key string) ([]byte, error) {
	fullKey := bucket + "/" + key
	data, ok := m.data[fullKey]
	if !ok {
		return nil, nil
	}
	return data, nil
}

func (m *mockMetadataDB) Set(bucket, key string, value []byte) error {
	fullKey := bucket + "/" + key
	m.data[fullKey] = value
	return nil
}

func (m *mockMetadataDB) SetWithTx(bucket, key string, value []byte, callback func() error) error {
	fullKey := bucket + "/" + key
	m.data[fullKey] = value
	if callback != nil {
		return callback()
	}
	return nil
}

func (m *mockMetadataDB) Delete(bucket, key string) error {
	fullKey := bucket + "/" + key
	delete(m.data, fullKey)
	return nil
}

func (m *mockMetadataDB) DeleteWithTx(bucket, key string, callback func() error) error {
	fullKey := bucket + "/" + key
	delete(m.data, fullKey)
	if callback != nil {
		return callback()
	}
	return nil
}

func (m *mockMetadataDB) List(bucket string) (map[string][]byte, error) {
	result := make(map[string][]byte)
	prefix := bucket + "/"
	for k, v := range m.data {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			key := k[len(prefix):]
			result[key] = v
		}
	}
	return result, nil
}

func (m *mockMetadataDB) ReadAll(bucket string) (map[string][]byte, error) {
	return m.List(bucket)
}

func (m *mockMetadataDB) GetBs(key string, buckets ...[]byte) ([]byte, error) {

	if len(buckets) > 0 {
		bucket := string(buckets[0])
		return m.Get(bucket, key)
	}
	return nil, nil
}

func (m *mockMetadataDB) ReadAllBs(buckets ...[]byte) (map[string][]byte, error) {

	if len(buckets) > 0 {
		bucket := string(buckets[0])
		return m.ReadAll(bucket)
	}
	return make(map[string][]byte), nil
}

func (m *mockMetadataDB) Close() error {
	return nil
}
