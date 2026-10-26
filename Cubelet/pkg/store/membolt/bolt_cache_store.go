// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package membolt

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sync"

	"github.com/containerd/errdefs"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/multimetadb/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/multimeta"
	"k8s.io/client-go/tools/cache"
)

type BoltCacheStore[Obj any] struct {
	cache.Indexer
	db         multimeta.MetadataDBAPI
	mu         sync.RWMutex
	bucketName string
	keyFunc    cache.KeyFunc
}

func NewBoltCacheStore[Obj any](db multimeta.MetadataDBAPI, keyFunc cache.KeyFunc, indexers cache.Indexers, obj Obj) (*BoltCacheStore[Obj], error) {
	c := cache.NewIndexer(keyFunc, indexers)

	t := reflect.TypeOf(obj)
	typeName := t.Name()

	if typeName == "" && t.Kind() == reflect.Ptr {
		typeName = t.Elem().Name()
	}

	if typeName == "" {
		typeName = "generic"
	}
	bucketName := fmt.Sprintf("generic_%s", typeName)

	multimeta.RegisterBucket(&multimeta.BucketDefineInternal{
		BucketDefine: &multimetadb.BucketDefine{
			Name:     bucketName,
			Describe: fmt.Sprintf("generic db for %s", typeName),
		},
		CubeStore: db,
	})
	store := &BoltCacheStore[Obj]{
		db:         db,
		Indexer:    c,
		bucketName: bucketName,
		keyFunc:    keyFunc,
	}
	err := store.Resync()
	if err != nil {
		return nil, fmt.Errorf("failed to resync store: %w", err)
	}
	return store, nil
}

func (bcs *BoltCacheStore[Obj]) Add(obj Obj) error {
	key, err := bcs.keyFunc(obj)
	if err != nil {
		return fmt.Errorf("failed to get key: %w", err)
	}

	data, err := json.Marshal(obj)
	if err != nil {
		return fmt.Errorf("failed to marshal object: %w", err)
	}

	if err := bcs.db.SetWithTx(bcs.bucketName, key, data, func() error {
		return bcs.Indexer.Add(obj)
	}); err != nil {
		return fmt.Errorf("failed to set in database: %w", err)
	}

	return nil
}

func (bcs *BoltCacheStore[Obj]) Update(obj Obj) error {
	key, err := bcs.keyFunc(obj)
	if err != nil {
		return fmt.Errorf("failed to get key: %w", err)
	}

	data, err := json.Marshal(obj)
	if err != nil {
		return fmt.Errorf("failed to marshal object: %w", err)
	}

	if err := bcs.db.SetWithTx(bcs.bucketName, key, data, func() error {
		return bcs.Indexer.Update(obj)
	}); err != nil {
		return fmt.Errorf("failed to set in database: %w", err)
	}

	return nil
}

func (bcs *BoltCacheStore[Obj]) Delete(obj Obj) error {
	key, err := bcs.keyFunc(obj)
	if err != nil {
		return fmt.Errorf("failed to get key: %w", err)
	}

	return bcs.DeleteByKey(key)
}

func (bcs *BoltCacheStore[Obj]) DeleteByKey(key string) error {
	v, exist, err := bcs.Indexer.GetByKey(key)
	if err != nil {
		return fmt.Errorf("failed to get by key: %w", err)
	}

	if err := bcs.db.DeleteWithTx(bcs.bucketName, key, func() error {
		if !exist {
			return nil
		}
		return bcs.Indexer.Delete(v)
	}); err != nil {
		return fmt.Errorf("failed to delete from database: %w", err)
	}
	return nil
}

func (bcs *BoltCacheStore[Obj]) Replace(list []interface{}, resourceVersion string) error {
	bcs.mu.Lock()
	defer bcs.mu.Unlock()

	allData, err := bcs.db.ReadAll(bcs.bucketName)
	if err == nil {
		for key := range allData {
			_ = bcs.db.DeleteWithTx(bcs.bucketName, key, nil)
		}
	}

	if err := bcs.Indexer.Replace(list, resourceVersion); err != nil {
		return fmt.Errorf("failed to replace cache: %w", err)
	}

	for _, item := range list {
		obj, ok := item.(Obj)
		if !ok {
			continue
		}

		key, err := bcs.keyFunc(obj)
		if err != nil {
			continue
		}

		data, err := json.Marshal(obj)
		if err != nil {
			continue
		}

		_ = bcs.db.SetWithTx(bcs.bucketName, key, data, nil)
	}

	return nil
}

func (bcs *BoltCacheStore[Obj]) Resync() error {
	bcs.mu.Lock()
	defer bcs.mu.Unlock()

	allData, err := bcs.db.ReadAll(bcs.bucketName)
	if err != nil {
		return fmt.Errorf("failed to read from database: %w", err)
	}

	bcs.Indexer.Replace([]interface{}{}, "")

	for _, data := range allData {
		var obj Obj
		if err := json.Unmarshal(data, &obj); err != nil {
			continue
		}
		_ = bcs.Indexer.Add(obj)
	}

	return nil
}

func (bcs *BoltCacheStore[Obj]) GetStore() cache.Store {
	return bcs.Indexer
}

func (bcs *BoltCacheStore[Obj]) GetDatabase() multimeta.MetadataDBAPI {
	return bcs.db
}

func (bcs *BoltCacheStore[Obj]) ByIndexGeneric(indexName, indexedValue string) ([]Obj, error) {
	bcs.mu.RLock()
	defer bcs.mu.RUnlock()

	items, err := bcs.Indexer.ByIndex(indexName, indexedValue)
	if err != nil {
		return nil, err
	}

	result := make([]Obj, 0, len(items))
	for _, item := range items {
		if obj, ok := item.(Obj); ok {
			result = append(result, obj)
		}
	}
	return result, nil
}

func (bcs *BoltCacheStore[Obj]) IndexGeneric(indexName string, obj Obj) ([]Obj, error) {
	bcs.mu.RLock()
	defer bcs.mu.RUnlock()

	items, err := bcs.Indexer.Index(indexName, obj)
	if err != nil {
		return nil, err
	}

	result := make([]Obj, 0, len(items))
	for _, item := range items {
		if o, ok := item.(Obj); ok {
			result = append(result, o)
		}
	}
	return result, nil
}

func (bcs *BoltCacheStore[Obj]) GetGeneric(key string) (Obj, error) {
	bcs.mu.RLock()
	defer bcs.mu.RUnlock()

	item, exists, err := bcs.Indexer.GetByKey(key)
	if err != nil {
		var zero Obj
		return zero, err
	}
	if !exists {
		var zero Obj
		return zero, errdefs.ErrNotFound
	}

	if obj, ok := item.(Obj); ok {
		return obj, nil
	}
	var zero Obj
	return zero, fmt.Errorf("object type mismatch for key: %s", key)
}

func (bcs *BoltCacheStore[Obj]) ListGeneric() ([]Obj, error) {
	bcs.mu.RLock()
	defer bcs.mu.RUnlock()

	items := bcs.Indexer.List()
	result := make([]Obj, 0, len(items))
	for _, item := range items {
		if obj, ok := item.(Obj); ok {
			result = append(result, obj)
		}
	}
	return result, nil
}

func (bcs *BoltCacheStore[Obj]) ListKeysGeneric() ([]string, error) {
	bcs.mu.RLock()
	defer bcs.mu.RUnlock()

	keys := bcs.Indexer.ListKeys()
	return keys, nil
}

func (bcs *BoltCacheStore[Obj]) GetIndexersGeneric() cache.Indexers {

	return bcs.Indexer.GetIndexers()
}

func (bcs *BoltCacheStore[Obj]) ListIndexFuncValuesGeneric(indexName string) ([]string, error) {
	values := bcs.Indexer.ListIndexFuncValues(indexName)
	return values, nil
}

func (bcs *BoltCacheStore[Obj]) IndexKeysGeneric(indexName, indexedValue string) ([]string, error) {
	bcs.mu.RLock()
	defer bcs.mu.RUnlock()

	keys, err := bcs.Indexer.IndexKeys(indexName, indexedValue)
	return keys, err
}
