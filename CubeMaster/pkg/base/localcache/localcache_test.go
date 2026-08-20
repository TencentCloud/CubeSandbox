// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package localcache cache data in local memory with rich features
package localcache

import (
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/patrickmn/go-cache"
	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/localcache/util"
)

func TestLocalCache(t *testing.T) {
	localCache := NewCache("test",
		func(ctx context.Context, key string) (val interface{}, found bool, err error) {

			start := time.Now()

			defer func() {
				fmt.Printf("--%d---\n", time.Since(start).Milliseconds())
			}()
			return RandString(10), true, nil
		},
		&LocalCacheConfig{
			ExpiredUse:    true,
			LowCacheSize:  1000000,
			HighCacheSize: 2000000,
			Expired:       5 * time.Second,
		})

	ctx := context.Background()
	ctxKey := "A"
	wg := &sync.WaitGroup{}
	for i := 0; i < 2700; i++ {

		wg.Add(1)
		iterCtx := context.WithValue(ctx, ctxKey, i)
		go func(c context.Context) {
			defer wg.Done()
			start := time.Now()
			localCache.Get(c, RandString(8))
			fmt.Printf("=====%d====\n", time.Since(start).Milliseconds())
		}(iterCtx)

	}
	wg.Wait()
}

func TestExpiredUse(t *testing.T) {

	once := true
	a := "Test"
	localCache := NewCache("test",
		func(ctx context.Context, key string) (val interface{}, found bool, err error) {
			if !once {
				a = "11"
			}

			fmt.Println("============")
			once = false
			return a, true, nil
		},
		&LocalCacheConfig{

			LowCacheSize:  1000000,
			HighCacheSize: 2000000,
			Expired:       2 * time.Second,
		})

	ctx := context.Background()
	v, found, err := localCache.Get(ctx, "a")
	if err != nil {
		fmt.Println(err)
	}
	assert.Equal(t, found, true)
	assert.Equal(t, v.(string), "Test")

	time.Sleep(2 * time.Second)
	v, found, err = localCache.Get(ctx, "a")
	if err != nil {
		fmt.Println(err)
	}
	assert.Equal(t, found, true)
	assert.Equal(t, v.(string), "11")
}

func TestNoValue(t *testing.T) {
	once := true
	itrm := ""
	found := false

	localCache := NewCache("test",
		func(ctx context.Context, key string) (val interface{}, found bool, err error) {
			if !once {
				time.Sleep(5 * time.Second)
				itrm = "Test"
				found = true
			}
			once = false
			return itrm, found, nil
		},
		&LocalCacheConfig{
			ExpiredUse:    true,
			LowCacheSize:  1000000,
			HighCacheSize: 2000000,
			Expired:       5 * time.Second,
		})

	ctx := context.Background()
	v, found, err := localCache.Get(ctx, "a")
	if err != nil {
		fmt.Println(err)
	}
	assert.Equal(t, false, found)
	assert.Equal(t, v, nil)

	time.Sleep(500 * time.Millisecond)
	v, found, err = localCache.Get(ctx, "a")
	if err != nil {
		fmt.Println(err)
	}
	assert.Equal(t, true, found)
	assert.Equal(t, v, nil)

	time.Sleep(6 * time.Second)
	v, found, err = localCache.Get(ctx, "a")
	if err != nil {
		fmt.Println(err)
	}
	assert.Equal(t, true, found)
	assert.Equal(t, v.(string), "Test")
}

func TestSaveFile(t *testing.T) {
	cacheFile := filepath.Join(t.TempDir(), "cache.gob")
	localCache := NewCache("test",
		func(ctx context.Context, key string) (val interface{}, found bool, err error) {
			return "Test", true, nil
		},
		&LocalCacheConfig{
			OpenCacheFile: true,
			LoadFileName:  cacheFile,
			ExpiredUse:    true,
			Expired:       5 * time.Second,
		})

	ctx := context.Background()
	localCache.Get(ctx, "a")
	localCache.Get(ctx, "b")
	localCache.Get(ctx, "c")
	localCache.Destroy()

	localCache = NewCache("test",
		func(ctx context.Context, key string) (val interface{}, found bool, err error) {
			fmt.Println("============")
			return "Test1", true, nil
		},
		&LocalCacheConfig{
			OpenCacheFile: true,
			LoadFileName:  cacheFile,
			ExpiredUse:    true,
			Expired:       5 * time.Second,
		})

	a, _ := json.Marshal(localCache.cache.Items())
	fmt.Println(string(a))

	v, found, err := localCache.Get(ctx, "a")
	if err != nil {
		fmt.Println(err)
	}
	assert.Equal(t, found, true)
	assert.Equal(t, v.(string), "Test")
}

func TestSaveFileDoesNotMutateLiveCache(t *testing.T) {
	valueList := list.New()
	cacheValue := &util.CacheValue{Key: "key", Value: "value"}
	liveCache := cache.New(0, 0)
	element := valueList.PushBack(cacheValue)
	liveCache.Set("key", element, cache.NoExpiration)
	localCache := &LocalCache{cache: liveCache}

	localCache.saveFile(filepath.Join(t.TempDir(), "cache.gob"))

	item, found := liveCache.Get("key")
	if assert.True(t, found) {
		assert.Same(t, element, item)
	}
	assert.Equal(t, 1, valueList.Len())
}

func TestSaveFileDoesNotPanicOnInvalidCacheEntries(t *testing.T) {
	valueList := list.New()
	liveCache := cache.New(0, 0)
	liveCache.Set("invalid-element", valueList.PushBack("invalid"), cache.NoExpiration)
	liveCache.Set("invalid-value", "invalid", cache.NoExpiration)
	localCache := &LocalCache{name: "test", cache: liveCache}

	assert.NotPanics(t, func() {
		localCache.saveFile(filepath.Join(t.TempDir(), "cache.gob"))
	})
}

func TestLoadFileFailurePreservesLiveCache(t *testing.T) {
	valueList := list.New()
	cacheValue := &util.CacheValue{Key: "key", Value: "value"}
	element := valueList.PushBack(cacheValue)
	liveCache := cache.New(0, 0)
	liveCache.Set("key", element, cache.NoExpiration)
	localCache := &LocalCache{name: "test", valueList: valueList, cache: liveCache}

	assert.NotPanics(t, func() {
		localCache.loadFile(filepath.Join(t.TempDir(), "missing.gob"))
	})

	item, found := liveCache.Get("key")
	if assert.True(t, found) {
		assert.Same(t, element, item)
	}
	assert.Equal(t, 1, valueList.Len())
	assert.Zero(t, localCache.curCacheSize)
}

func RandString(len int) string {
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	bytes := make([]byte, len)
	for i := 0; i < len; i++ {
		b := r.Intn(26) + 65
		bytes[i] = byte(b)
	}
	return string(bytes)
}
