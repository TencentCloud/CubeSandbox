// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package membolt

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"k8s.io/client-go/tools/cache"
)

type TestObject struct {
	Name  string `json:"name"`
	Value int    `json:"value"`
}

func (to TestObject) Key() string {
	return to.Name
}

func TestBoltCacheStoreAdd(t *testing.T) {

	tmpDir := t.TempDir()
	db, err := utils.NewCubeStore(tmpDir+"/test.db", nil)
	if err != nil {
		t.Fatalf("failed to create database: %v", err)
	}
	defer db.Close()

	keyFunc := func(obj interface{}) (string, error) {
		o := obj.(*TestObject)
		return o.Name, nil
	}
	store, err := NewBoltCacheStore(db, keyFunc, cache.Indexers{}, &TestObject{})
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	obj := &TestObject{Name: "test1", Value: 100}
	if err := store.Add(obj); err != nil {
		t.Fatalf("failed to add object: %v", err)
	}

	item, exists, err := store.GetByKey("test1")
	if err != nil || !exists {
		t.Fatalf("object not found in cache: exists=%v, err=%v", exists, err)
	}

	retrieved := item.(*TestObject)
	if retrieved.Name != "test1" || retrieved.Value != 100 {
		t.Fatalf("object mismatch: got %+v", retrieved)
	}

	assert.Equal(t, "generic_TestObject", store.bucketName)
	data, err := db.Get("generic_TestObject", "test1")
	if err != nil {
		t.Fatalf("object not found in database: %v", err)
	}

	var dbObj TestObject
	if err := json.Unmarshal(data, &dbObj); err != nil {
		t.Fatalf("failed to unmarshal database object: %v", err)
	}

	if dbObj.Name != "test1" || dbObj.Value != 100 {
		t.Fatalf("database object mismatch: got %+v", dbObj)
	}
}

func TestBoltCacheStoreUpdate(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := utils.NewCubeStore(tmpDir+"/test.db", nil)
	if err != nil {
		t.Fatalf("failed to create database: %v", err)
	}
	defer db.Close()

	keyFunc := func(obj interface{}) (string, error) {
		o := obj.(TestObject)
		return o.Name, nil
	}
	store, err := NewBoltCacheStore(db, keyFunc, cache.Indexers{}, TestObject{})
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	obj := TestObject{Name: "test2", Value: 100}
	if err := store.Add(obj); err != nil {
		t.Fatalf("failed to add object: %v", err)
	}

	obj.Value = 200
	if err := store.Update(obj); err != nil {
		t.Fatalf("failed to update object: %v", err)
	}

	item, exists, err := store.GetByKey("test2")
	if err != nil || !exists {
		t.Fatalf("object not found in cache after update")
	}

	retrieved := item.(TestObject)
	if retrieved.Value != 200 {
		t.Fatalf("cache object not updated: got value %d, expected 200", retrieved.Value)
	}

	data, err := db.Get("generic_TestObject", "test2")
	if err != nil {
		t.Fatalf("object not found in database after update: %v", err)
	}

	var dbObj TestObject
	if err := json.Unmarshal(data, &dbObj); err != nil {
		t.Fatalf("failed to unmarshal database object: %v", err)
	}

	if dbObj.Value != 200 {
		t.Fatalf("database object not updated: got value %d, expected 200", dbObj.Value)
	}
}

func TestBoltCacheStoreDelete(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := utils.NewCubeStore(tmpDir+"/test.db", nil)
	if err != nil {
		t.Fatalf("failed to create database: %v", err)
	}
	defer db.Close()

	keyFunc := func(obj interface{}) (string, error) {
		o := obj.(TestObject)
		return o.Name, nil
	}
	store, err := NewBoltCacheStore(db, keyFunc, cache.Indexers{}, TestObject{})
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	obj := TestObject{Name: "test3", Value: 100}
	if err := store.Add(obj); err != nil {
		t.Fatalf("failed to add object: %v", err)
	}

	if err := store.Delete(obj); err != nil {
		t.Fatalf("failed to delete object: %v", err)
	}

	_, exists, err := store.GetByKey("test3")
	if exists {
		t.Fatalf("object still exists in cache after delete")
	}

	_, err = db.Get("generic_TestObject", "test3")
	if err != utils.ErrorKeyNotFound {
		t.Fatalf("object still exists in database after delete: %v", err)
	}
}

func TestBoltCacheStoreList(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := utils.NewCubeStore(tmpDir+"/test.db", nil)
	if err != nil {
		t.Fatalf("failed to create database: %v", err)
	}
	defer db.Close()

	keyFunc := func(obj interface{}) (string, error) {
		o := obj.(TestObject)
		return o.Name, nil
	}
	store, err := NewBoltCacheStore(db, keyFunc, cache.Indexers{}, TestObject{})
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	for i := 1; i <= 3; i++ {
		obj := TestObject{Name: "test" + string(rune('0'+i)), Value: i * 100}
		if err := store.Add(obj); err != nil {
			t.Fatalf("failed to add object: %v", err)
		}
	}

	items := store.List()
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items))
	}

	keys := store.ListKeys()
	if len(keys) != 3 {
		t.Fatalf("expected 3 keys, got %d", len(keys))
	}
}

func TestBoltCacheStoreResync(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := utils.NewCubeStore(tmpDir+"/test.db", nil)
	if err != nil {
		t.Fatalf("failed to create database: %v", err)
	}
	defer db.Close()

	keyFunc := func(obj interface{}) (string, error) {
		o := obj.(TestObject)
		return o.Name, nil
	}
	store, err := NewBoltCacheStore(db, keyFunc, cache.Indexers{}, TestObject{})
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	obj := TestObject{Name: "test4", Value: 100}
	if err := store.Add(obj); err != nil {
		t.Fatalf("failed to add object: %v", err)
	}

	store.Indexer.Replace([]interface{}{}, "")

	items := store.List()
	if len(items) != 0 {
		t.Fatalf("cache should be empty, got %d items", len(items))
	}

	if err := store.Resync(); err != nil {
		t.Fatalf("failed to resync: %v", err)
	}

	items = store.List()
	if len(items) != 1 {
		t.Fatalf("expected 1 item after resync, got %d", len(items))
	}

	item := items[0].(TestObject)
	if item.Name != "test4" || item.Value != 100 {
		t.Fatalf("resync object mismatch: got %+v", item)
	}
}

func TestBoltCacheStoreReplace(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := utils.NewCubeStore(tmpDir+"/test.db", nil)
	if err != nil {
		t.Fatalf("failed to create database: %v", err)
	}
	defer db.Close()

	keyFunc := func(obj interface{}) (string, error) {
		o := obj.(TestObject)
		return o.Name, nil
	}
	store, err := NewBoltCacheStore(db, keyFunc, cache.Indexers{}, TestObject{})
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	obj1 := TestObject{Name: "old1", Value: 100}
	if err := store.Add(obj1); err != nil {
		t.Fatalf("failed to add object: %v", err)
	}

	newList := []interface{}{
		TestObject{Name: "new1", Value: 200},
		TestObject{Name: "new2", Value: 300},
	}
	if err := store.Replace(newList, ""); err != nil {
		t.Fatalf("failed to replace: %v", err)
	}

	items := store.List()
	if len(items) != 2 {
		t.Fatalf("expected 2 items after replace, got %d", len(items))
	}

	allData, err := db.ReadAll("generic_TestObject")
	if err != nil {
		t.Fatalf("failed to read from database: %v", err)
	}

	if len(allData) != 2 {
		t.Fatalf("expected 2 items in database after replace, got %d", len(allData))
	}

	_, err = db.Get("generic_TestObject", "old1")
	if err != utils.ErrorKeyNotFound {
		t.Fatalf("old object still exists in database after replace")
	}
}

func BenchmarkBoltCacheStoreAdd(b *testing.B) {
	tmpDir := b.TempDir()
	db, err := utils.NewCubeStore(tmpDir+"/test.db", nil)
	if err != nil {
		b.Fatalf("failed to create database: %v", err)
	}
	defer db.Close()

	keyFunc := func(obj interface{}) (string, error) {
		o := obj.(TestObject)
		return o.Name, nil
	}
	store, err := NewBoltCacheStore(db, keyFunc, cache.Indexers{}, TestObject{})
	if err != nil {
		b.Fatalf("failed to create store: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		obj := TestObject{Name: "test" + string(rune('0'+(i%10))), Value: i}
		_ = store.Add(obj)
	}
}

func BenchmarkBoltCacheStoreUpdate(b *testing.B) {
	tmpDir := b.TempDir()
	db, err := utils.NewCubeStore(tmpDir+"/test.db", nil)
	if err != nil {
		b.Fatalf("failed to create database: %v", err)
	}
	defer db.Close()

	keyFunc := func(obj interface{}) (string, error) {
		o := obj.(TestObject)
		return o.Name, nil
	}
	store, err := NewBoltCacheStore(db, keyFunc, cache.Indexers{}, &TestObject{})
	if err != nil {
		b.Fatalf("failed to create store: %v", err)
	}

	for i := 0; i < 10; i++ {
		obj := &TestObject{Name: "test" + string(rune('0'+i)), Value: i}
		_ = store.Add(obj)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		obj := &TestObject{Name: "test" + string(rune('0'+(i%10))), Value: i}
		_ = store.Update(obj)
	}
}

func TestBoltCacheStoreWithIndexers(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := utils.NewCubeStore(tmpDir+"/test.db", nil)
	if err != nil {
		t.Fatalf("failed to create database: %v", err)
	}
	defer db.Close()

	valueIndexFunc := func(obj interface{}) ([]string, error) {
		to := obj.(TestObject)
		return []string{string(rune('0' + (to.Value % 10)))}, nil
	}

	prefixIndexFunc := func(obj interface{}) ([]string, error) {
		to := obj.(TestObject)
		if len(to.Name) > 4 {
			return []string{to.Name[:4]}, nil
		}
		return []string{}, nil
	}

	indexers := cache.Indexers{
		"value":  valueIndexFunc,
		"prefix": prefixIndexFunc,
	}

	keyFunc := func(obj interface{}) (string, error) {
		o := obj.(TestObject)
		return o.Name, nil
	}

	store, err := NewBoltCacheStore(db, keyFunc, indexers, TestObject{})
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	testObjects := []TestObject{
		{Name: "test1", Value: 12},
		{Name: "test2", Value: 22},
		{Name: "test3", Value: 13},
		{Name: "test4", Value: 14},
		{Name: "test5", Value: 15},
	}

	for _, obj := range testObjects {
		if err := store.Add(obj); err != nil {
			t.Fatalf("failed to add object: %v", err)
		}
	}

	items := store.List()
	if len(items) != 5 {
		t.Fatalf("expected 5 items, got %d", len(items))
	}

	indexedItems, err := store.ByIndex("value", "2")
	if err != nil {
		t.Fatalf("failed to index by value: %v", err)
	}
	if len(indexedItems) != 2 {
		t.Fatalf("expected 2 items with value index '2', got %d", len(indexedItems))
	}

	prefixItems, err := store.ByIndex("prefix", "test")
	if err != nil {
		t.Fatalf("failed to index by prefix: %v", err)
	}
	if len(prefixItems) != 5 {
		t.Fatalf("expected 5 items with prefix index 'test', got %d", len(prefixItems))
	}

	for _, item := range indexedItems {
		obj := item.(TestObject)
		if obj.Value%10 != 2 {
			t.Fatalf("indexed object has wrong value: %d, expected value mod 10 == 2", obj.Value)
		}
	}

	t.Logf("✓ Multi-index test passed: found %d items with value index '0'", len(indexedItems))
}

func TestBoltCacheStoreIndexerUpdate(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := utils.NewCubeStore(tmpDir+"/test.db", nil)
	if err != nil {
		t.Fatalf("failed to create database: %v", err)
	}
	defer db.Close()

	valueIndexFunc := func(obj interface{}) ([]string, error) {
		to := obj.(TestObject)
		return []string{string(rune('0' + (to.Value % 10)))}, nil
	}

	indexers := cache.Indexers{
		"value": valueIndexFunc,
	}

	keyFunc := func(obj interface{}) (string, error) {
		o := obj.(TestObject)
		return o.Name, nil
	}

	store, err := NewBoltCacheStore(db, keyFunc, indexers, TestObject{})
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	obj := TestObject{Name: "item1", Value: 5}
	if err := store.Add(obj); err != nil {
		t.Fatalf("failed to add object: %v", err)
	}

	items, err := store.ByIndex("value", "5")
	if err != nil {
		t.Fatalf("failed to index: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item with value index '5', got %d", len(items))
	}

	obj.Value = 17
	if err := store.Update(obj); err != nil {
		t.Fatalf("failed to update object: %v", err)
	}

	oldItems, err := store.ByIndex("value", "5")
	if err != nil {
		t.Fatalf("failed to index: %v", err)
	}
	if len(oldItems) != 0 {
		t.Fatalf("expected 0 items with old value index '5', got %d", len(oldItems))
	}

	newItems, err := store.ByIndex("value", "7")
	if err != nil {
		t.Fatalf("failed to index: %v", err)
	}
	if len(newItems) != 1 {
		t.Fatalf("expected 1 item with new value index '7', got %d", len(newItems))
	}

	t.Logf("✓ Indexer update test passed")
}

func TestBoltCacheStoreMultipleIndexers(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := utils.NewCubeStore(tmpDir+"/test.db", nil)
	if err != nil {
		t.Fatalf("failed to create database: %v", err)
	}
	defer db.Close()

	rangeIndexFunc := func(obj interface{}) ([]string, error) {
		to := obj.(TestObject)
		if to.Value < 50 {
			return []string{"low"}, nil
		} else if to.Value < 100 {
			return []string{"medium"}, nil
		}
		return []string{"high"}, nil
	}

	lengthIndexFunc := func(obj interface{}) ([]string, error) {
		to := obj.(TestObject)
		return []string{string(rune('0' + len(to.Name)))}, nil
	}

	parityIndexFunc := func(obj interface{}) ([]string, error) {
		to := obj.(TestObject)
		if to.Value%2 == 0 {
			return []string{"even"}, nil
		}
		return []string{"odd"}, nil
	}

	indexers := cache.Indexers{
		"range":  rangeIndexFunc,
		"length": lengthIndexFunc,
		"parity": parityIndexFunc,
	}

	keyFunc := func(obj interface{}) (string, error) {
		o := obj.(TestObject)
		return o.Name, nil
	}

	store, err := NewBoltCacheStore(db, keyFunc, indexers, TestObject{})
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	testObjects := []TestObject{
		{Name: "obj1", Value: 25},
		{Name: "obj2", Value: 50},
		{Name: "obj3", Value: 75},
		{Name: "obj4", Value: 100},
		{Name: "obj5", Value: 150},
	}

	for _, obj := range testObjects {
		if err := store.Add(obj); err != nil {
			t.Fatalf("failed to add object: %v", err)
		}
	}

	tests := []struct {
		indexName string
		indexKey  string
		expected  int
	}{
		{"range", "low", 1},
		{"range", "medium", 2},
		{"range", "high", 2},
		{"parity", "even", 3},
		{"parity", "odd", 2},
		{"length", "4", 5},
	}

	for _, test := range tests {
		items, err := store.ByIndex(test.indexName, test.indexKey)
		if err != nil {
			t.Fatalf("failed to index by %s: %v", test.indexName, err)
		}
		if len(items) != test.expected {
			t.Fatalf("index %s:%s expected %d items, got %d", test.indexName, test.indexKey, test.expected, len(items))
		}
	}

	t.Logf("✓ Multiple indexers test passed")
}

func TestBoltCacheStoreIndexerWithDelete(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := utils.NewCubeStore(tmpDir+"/test.db", nil)
	if err != nil {
		t.Fatalf("failed to create database: %v", err)
	}
	defer db.Close()

	valueIndexFunc := func(obj interface{}) ([]string, error) {
		to := obj.(TestObject)
		return []string{string(rune('0' + (to.Value % 10)))}, nil
	}

	indexers := cache.Indexers{
		"value": valueIndexFunc,
	}

	keyFunc := func(obj interface{}) (string, error) {
		o := obj.(TestObject)
		return o.Name, nil
	}

	store, err := NewBoltCacheStore(db, keyFunc, indexers, TestObject{})
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	objects := []TestObject{
		{Name: "item1", Value: 5},
		{Name: "item2", Value: 15},
		{Name: "item3", Value: 25},
	}

	for _, obj := range objects {
		if err := store.Add(obj); err != nil {
			t.Fatalf("failed to add object: %v", err)
		}
	}

	items, err := store.ByIndex("value", "5")
	if err != nil {
		t.Fatalf("failed to index: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 items with value index '5', got %d", len(items))
	}

	if err := store.Delete(objects[0]); err != nil {
		t.Fatalf("failed to delete object: %v", err)
	}

	items, err = store.ByIndex("value", "5")
	if err != nil {
		t.Fatalf("failed to index: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items after delete, got %d", len(items))
	}

	t.Logf("✓ Indexer delete test passed")
}

func TestBoltCacheStoreGenericMethods(t *testing.T) {

	tmpDir := t.TempDir()
	db, err := utils.NewCubeStore(tmpDir+"/test.db", nil)
	if err != nil {
		t.Fatalf("failed to create database: %v", err)
	}
	defer db.Close()

	keyFunc := func(obj interface{}) (string, error) {
		o := obj.(TestObject)
		return o.Name, nil
	}

	indexers := cache.Indexers{
		"value": func(obj interface{}) ([]string, error) {
			o := obj.(TestObject)
			return []string{string(rune('0' + o.Value%10))}, nil
		},
	}

	store, err := NewBoltCacheStore(db, keyFunc, indexers, TestObject{})
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	objects := []TestObject{
		{Name: "obj1", Value: 12},
		{Name: "obj2", Value: 22},
		{Name: "obj3", Value: 13},
	}

	for _, obj := range objects {
		if err := store.Add(obj); err != nil {
			t.Fatalf("failed to add object: %v", err)
		}
	}

	t.Run("ListGeneric", func(t *testing.T) {
		items, err := store.ListGeneric()
		if err != nil {
			t.Fatalf("failed to list: %v", err)
		}
		if len(items) != 3 {
			t.Fatalf("expected 3 items, got %d", len(items))
		}

		for _, item := range items {
			if item.Name == "" {
				t.Fatalf("invalid object in list")
			}
		}
		t.Logf("✓ ListGeneric returned %d items with correct type", len(items))
	})

	t.Run("ByIndexGeneric", func(t *testing.T) {
		items, err := store.ByIndexGeneric("value", "2")
		if err != nil {
			t.Fatalf("failed to query by index: %v", err)
		}
		if len(items) != 2 {
			t.Fatalf("expected 2 items with value%%10==2, got %d", len(items))
		}

		for _, item := range items {
			if item.Value%10 != 2 {
				t.Fatalf("invalid item in index result: Value=%d, Value%%10=%d", item.Value, item.Value%10)
			}
		}
		t.Logf("✓ ByIndexGeneric returned correct items with type safety")
	})

	t.Run("ListKeysGeneric", func(t *testing.T) {
		keys, err := store.ListKeysGeneric()
		if err != nil {
			t.Fatalf("failed to list keys: %v", err)
		}
		if len(keys) != 3 {
			t.Fatalf("expected 3 keys, got %d", len(keys))
		}
		t.Logf("✓ ListKeysGeneric returned %d keys", len(keys))
	})

	t.Run("ListIndexFuncValuesGeneric", func(t *testing.T) {
		values, err := store.ListIndexFuncValuesGeneric("value")
		if err != nil {
			t.Fatalf("failed to list index values: %v", err)
		}
		if len(values) == 0 {
			t.Fatalf("expected non-empty index values")
		}
		t.Logf("✓ ListIndexFuncValuesGeneric returned %d values", len(values))
	})

	t.Run("IndexKeysGeneric", func(t *testing.T) {
		keys, err := store.IndexKeysGeneric("value", "2")
		if err != nil {
			t.Fatalf("failed to get index keys: %v", err)
		}
		if len(keys) != 2 {
			t.Fatalf("expected 2 keys for value==2, got %d", len(keys))
		}
		t.Logf("✓ IndexKeysGeneric returned %d keys", len(keys))
	})

	t.Run("IndexGeneric", func(t *testing.T) {
		queryObj := TestObject{Name: "query", Value: 12}
		items, err := store.IndexGeneric("value", queryObj)
		if err != nil {
			t.Fatalf("failed to index: %v", err)
		}
		if len(items) != 2 {
			t.Fatalf("expected 2 items with value%%10==2, got %d", len(items))
		}
		t.Logf("✓ IndexGeneric returned correct items with type safety")
	})

	t.Logf("✓ All generic methods tests passed")
}

func TestBoltCacheStoreTypeNameExtraction(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := utils.NewCubeStore(tmpDir+"/test.db", nil)
	if err != nil {
		t.Fatalf("failed to create database: %v", err)
	}
	defer db.Close()

	keyFunc := func(obj interface{}) (string, error) {
		return "key", nil
	}

	t.Run("非指针类型", func(t *testing.T) {

		store, err := NewBoltCacheStore(db, keyFunc, cache.Indexers{}, TestObject{})
		if err != nil {
			t.Fatalf("failed to create store: %v", err)
		}
		expected := "generic_TestObject"
		if store.bucketName != expected {
			t.Errorf("expected bucket name %s, got %s", expected, store.bucketName)
		}
		t.Logf("✓ Non-pointer type bucket name: %s", store.bucketName)
	})

	t.Run("指针类型", func(t *testing.T) {

		store, err := NewBoltCacheStore(db, keyFunc, cache.Indexers{}, &TestObject{})
		if err != nil {
			t.Fatalf("failed to create store: %v", err)
		}
		expected := "generic_TestObject"
		if store.bucketName != expected {
			t.Errorf("expected bucket name %s, got %s", expected, store.bucketName)
		}
		t.Logf("✓ Pointer type bucket name: %s", store.bucketName)
	})

	t.Run("类型反射验证", func(t *testing.T) {

		objPtr := &TestObject{}
		reflectType := reflect.TypeOf(objPtr)

		if reflectType.Name() != "" {
			t.Errorf("pointer type Name() should be empty, got %s", reflectType.Name())
		}

		if reflectType.Elem().Name() != "TestObject" {
			t.Errorf("pointer Elem().Name() should be TestObject, got %s", reflectType.Elem().Name())
		}

		t.Logf("✓ Type reflection verified: pointer Name() = \"%s\", Elem().Name() = \"%s\"", reflectType.Name(), reflectType.Elem().Name())
	})
}
