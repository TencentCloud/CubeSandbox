// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package localcache cache data in local memory with rich features
package localcache

import (
	"container/list"
	"context"
	"encoding/gob"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patrickmn/go-cache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/localcache/util"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"golang.org/x/time/rate"
)

type LoaderFunc func(context.Context, string) (interface{}, bool, error)

var (
	DefaultLowSize = int64((1 << 30) * 1)

	DefaultHighSize = int64((1 << 30) * 2)

	DefaultNullValueCacheExpired = 1 * time.Second

	DefaultAsyncRefreshBefore = 3 * time.Second

	DefaultMaxAsyncRefreshNum = 10

	DefaultCacheExpiredRemove = 24 * time.Hour
)

type LocalCacheConfig struct {
	ExpiredUse            bool          `yaml:"expired_use"`
	DemotionExpiredUse    bool          `yaml:"demotion_expired_use"`
	LowCacheSize          int64         `yaml:"low_cache_size"`
	HighCacheSize         int64         `yaml:"high_cache_size"`
	Expired               time.Duration `yaml:"expired"`
	NullValueCacheExpired time.Duration `yaml:"null_value_cache_expired"`
	AsyncRefreshBefore    time.Duration `yaml:"async_refresh_before"`
	CacheExpiredRemove    time.Duration `yaml:"cache_expired_remove"`
	MaxAsyncRefreshNum    int           `yaml:"max_async_refresh_num"`
	MaxConsecutiveFailNum int           `yaml:"max_consecutive_fail_num"`

	OpenCacheFile bool   `yaml:"open_cache_file"`
	LoadFileName  string `yaml:"load_file_name"`
}

type LocalCache struct {
	sync.Mutex
	waitGroup          sync.WaitGroup
	name               string
	curCacheSize       int64
	curLoaderNum       sync.Map
	chCacheExit        chan bool
	chShrinkCache      chan bool
	valueList          *list.List
	cache              *cache.Cache
	loader             LoaderFunc
	localCacheConfig   *LocalCacheConfig
	sharedCalls        util.SharedCalls
	consecutiveFailNum int64
	expiredUse         atomic.Bool
	destroyOnce        sync.Once
}

func NewCache(name string, loader LoaderFunc, localCacheConfig *LocalCacheConfig) *LocalCache {
	localCache := new(LocalCache)
	localCache.name = name
	localCache.valueList = list.New()
	localCache.cache = cache.New(0, 0)

	if localCacheConfig.OpenCacheFile {
		localCache.loadFile(localCacheConfig.LoadFileName)
	}
	localCache.localCacheConfig = localCache.SetupConfig(localCacheConfig)
	localCache.expiredUse.Store(localCache.localCacheConfig != nil && localCache.localCacheConfig.ExpiredUse)

	localCache.chShrinkCache = make(chan bool, 1)
	localCache.chCacheExit = make(chan bool)
	localCache.loader = loader
	localCache.sharedCalls = util.NewSharedCalls()
	localCache.waitGroup.Add(1)
	go localCache.shrinkCache()
	go localCache.errStrategy()
	CubeLog.Infof(`Cache create with configure:
    --------------------------
    | name:"%s"
    | low:%dbyte=%.3fMb
    | high:%dbyte=%.3fMb
    --------------------------`,
		localCache.name, localCache.localCacheConfig.LowCacheSize,
		float64(localCache.localCacheConfig.LowCacheSize)/1024/1024,
		localCache.localCacheConfig.HighCacheSize,
		float64(localCache.localCacheConfig.HighCacheSize)/1024/1024)
	return localCache
}

func (localCache *LocalCache) Destroy() {
	if localCache == nil {
		return
	}
	if localCache.chCacheExit == nil {
		return
	}
	localCache.destroyOnce.Do(func() {
		CubeLog.Infof("LruCache(%s) Destroy", localCache.name)
		if localCache.localCacheConfig.OpenCacheFile {
			localCache.saveFile(localCache.localCacheConfig.LoadFileName)
		}
		localCache.cache.Flush()
		close(localCache.chCacheExit)
		localCache.waitGroup.Wait()
	})
}

func (localCache *LocalCache) Get(ctx context.Context, key string) (interface{}, bool, error) {
	item, found := localCache.cache.Get(key)
	if found {
		element := item.(*list.Element)

		localCache.Lock()
		itm := element.Value.(*util.CacheValue)
		localCache.Unlock()

		if time.Now().Add(-itm.Expired).After(time.Unix(itm.LastAccess, 0)) {

			if !localCache.expiredUse.Load() {
				r, f, err := localCache.loadAndRefresh(ctx, key)

				if err != nil && localCache.localCacheConfig.DemotionExpiredUse {

					localCache.Lock()
					localCache.valueList.MoveToBack(element)
					localCache.Unlock()
					return itm.Value, true, nil
				}

				return r, f, err
			}

			localCache.asyncRefresh(ctx, key)
		} else if time.Now().Add(-itm.Expired).Add(localCache.localCacheConfig.AsyncRefreshBefore).
			After(time.Unix(itm.LastAccess, 0)) {

			localCache.asyncRefresh(ctx, key)
		}

		localCache.Lock()
		localCache.valueList.MoveToBack(element)
		localCache.Unlock()
		return itm.Value, true, nil
	}

	if localCache.loader != nil {
		return localCache.loadAndRefresh(ctx, key)
	}

	return nil, false, nil
}

func (localCache *LocalCache) put(key string, val interface{}, expired time.Duration) {
	if item, found := localCache.cache.Get(key); found {
		element := item.(*list.Element)
		localCache.Lock()
		prev := element.Value.(*util.CacheValue)
		next := &util.CacheValue{
			Key:        prev.Key,
			Value:      val,
			LastAccess: time.Now().Unix(),
			Expired:    expired}
		element.Value = next
		localCache.valueList.MoveToBack(element)
		localCache.Unlock()
		atomic.AddInt64(&localCache.curCacheSize, -prev.Size())
		atomic.AddInt64(&localCache.curCacheSize, next.Size())
	} else {
		itm := &util.CacheValue{
			Key:        key,
			Value:      val,
			LastAccess: time.Now().Unix(),
			Expired:    expired}
		atomic.AddInt64(&localCache.curCacheSize, itm.Size())
		localCache.Lock()
		element := localCache.valueList.PushBack(itm)
		localCache.Unlock()
		localCache.cache.Set(key, element, -1)
	}

	if atomic.LoadInt64(&localCache.curCacheSize) >= localCache.localCacheConfig.HighCacheSize {
		select {
		case localCache.chShrinkCache <- true:
		default:
		}
	}
}

func (localCache *LocalCache) loadAndRefresh(ctx context.Context, key string) (interface{}, bool, error) {
	if localCache.loader != nil {
		v, err := localCache.sharedCalls.Do(key, func() (i interface{}, e error) {
			item, found, err := localCache.loader(ctx, key)
			if err != nil {
				item, found, err = localCache.loader(ctx, key)
			}

			if err != nil {
				atomic.AddInt64(&localCache.consecutiveFailNum, 1)
				CubeLog.Errorf("Cache LoadAndRefresh Error:%s, %v, %s", key, found, err)
				return nil, err
			} else {
				atomic.StoreInt64(&localCache.consecutiveFailNum, 0)
			}

			if !found {
				localCache.put(key, nil, localCache.localCacheConfig.NullValueCacheExpired)
				return nil, nil
			}

			localCache.put(key, item, localCache.localCacheConfig.Expired)
			return item, nil
		})

		return v, v != nil, err
	}

	return nil, false, nil
}

func (localCache *LocalCache) asyncRefresh(ctx context.Context, key string) {
	v, ok := localCache.curLoaderNum.Load(key)
	if !ok || v == nil {
		v, _ = localCache.curLoaderNum.LoadOrStore(key,
			rate.NewLimiter(rate.Limit(localCache.localCacheConfig.MaxAsyncRefreshNum),
				localCache.localCacheConfig.MaxAsyncRefreshNum))
	}

	cn, _ := v.(*rate.Limiter)
	if cn.AllowN(time.Now(), 1) {
		go func() {
			_, _, _ = localCache.loadAndRefresh(ctx, key)
		}()
	}
}

func (localCache *LocalCache) shrinkCache() {
	defer localCache.waitGroup.Done()
	defer CubeLog.Infof("Cache Shrink Proccess goroutine End")
	ticker := time.NewTicker(time.Duration(15) * time.Minute)
	for {
		select {
		case <-localCache.chShrinkCache:
			curTime := time.Now()
			curCacheSize := atomic.LoadInt64(&localCache.curCacheSize)
			var shrinkNum, shrinkSize, size int64
			for {
				if atomic.LoadInt64(&localCache.curCacheSize) > localCache.localCacheConfig.LowCacheSize {
					localCache.Lock()
					element := localCache.valueList.Front()
					if element == nil {
						localCache.Unlock()
						break
					}
					Value := localCache.valueList.Remove(element)
					localCache.Unlock()
					itm := Value.(*util.CacheValue)
					size = itm.Size()
					atomic.AddInt64(&localCache.curCacheSize, -itm.Size())
					localCache.cache.Delete(itm.Key)
					shrinkSize += size
					shrinkNum++
				} else {
					break
				}
			}
			if shrinkSize > 0 {
				CubeLog.Infof("Cache(%s) ShrinkCache curSize:%d(byte),shrinkSize:%d(byte), shrinkObj:%d, usetime(%v)",
					localCache.name, curCacheSize, shrinkSize, shrinkNum, time.Since(curTime))
			}
		case <-localCache.chCacheExit:
			return
		case <-ticker.C:
			for {
				localCache.Lock()
				element := localCache.valueList.Front()
				if element == nil {
					localCache.Unlock()
					break
				}

				itm := element.Value.(*util.CacheValue)
				if time.Since(time.Unix(itm.LastAccess, 0)) > localCache.localCacheConfig.CacheExpiredRemove {
					localCache.valueList.Remove(element)
					localCache.Unlock()
					atomic.AddInt64(&localCache.curCacheSize, -itm.Size())
					localCache.cache.Delete(itm.Key)
				} else {
					localCache.Unlock()
					break
				}
			}
		}
	}
}

func (localCache *LocalCache) errStrategy() {
	ticker := time.NewTicker(time.Duration(10) * time.Second)
	for {
		select {
		case <-localCache.chCacheExit:
			return
		case <-ticker.C:

			if localCache.localCacheConfig.MaxConsecutiveFailNum > 0 {
				if atomic.LoadInt64(&localCache.consecutiveFailNum) >
					int64(localCache.localCacheConfig.MaxConsecutiveFailNum) {

					if localCache.localCacheConfig.DemotionExpiredUse {
						localCache.expiredUse.Store(true)
					}
				} else {

					if localCache.localCacheConfig.DemotionExpiredUse && localCache.expiredUse.Load() {
						localCache.expiredUse.Store(false)
					}
				}
			}
		}
	}
}

func (localCache *LocalCache) saveFile(file string) {
	snapshot := cache.New(0, 0)
	for key, item := range localCache.cache.Items() {
		var itm *util.CacheValue
		switch value := item.Object.(type) {
		case *list.Element:
			localCache.Lock()
			stored := value.Value
			localCache.Unlock()
			var ok bool
			itm, ok = stored.(*util.CacheValue)
			if !ok {
				CubeLog.Errorf("Cache(%s) cannot persist key %s: list element contains %T", localCache.name, key, stored)
				continue
			}
		case *util.CacheValue:
			itm = value
		default:
			CubeLog.Errorf("Cache(%s) cannot persist key %s: unsupported value type %T", localCache.name, key, item.Object)
			continue
		}
		snapshot.Set(key, itm, -1)
	}

	if err := snapshot.SaveFile(file); err != nil {
		CubeLog.Errorf("Cache(%s) failed to save cache file %s: %v", localCache.name, file, err)
	}
}

func (localCache *LocalCache) loadFile(file string) {
	gob.Register(&util.CacheValue{})
	if err := localCache.cache.LoadFile(file); err != nil {
		CubeLog.Errorf("Cache(%s) failed to load cache file %s: %v", localCache.name, file, err)
		return
	}
	for key, item := range localCache.cache.Items() {
		cacheValue := item.Object.(*util.CacheValue)
		atomic.AddInt64(&localCache.curCacheSize, cacheValue.Size())
		localCache.Lock()
		element := localCache.valueList.PushBack(cacheValue)
		localCache.Unlock()
		localCache.cache.Set(key, element, -1)
	}
}

func (localCache *LocalCache) SetupConfig(localCacheConfig *LocalCacheConfig) *LocalCacheConfig {
	if localCacheConfig == nil {
		return nil
	}

	if localCacheConfig.HighCacheSize == 0 {
		localCacheConfig.HighCacheSize = DefaultHighSize
	}
	if localCacheConfig.LowCacheSize == 0 {
		localCacheConfig.LowCacheSize = DefaultLowSize
	}
	if localCacheConfig.AsyncRefreshBefore == 0 {
		localCacheConfig.AsyncRefreshBefore = DefaultAsyncRefreshBefore
	}
	if localCacheConfig.MaxAsyncRefreshNum == 0 {
		localCacheConfig.MaxAsyncRefreshNum = DefaultMaxAsyncRefreshNum
	}
	if localCacheConfig.NullValueCacheExpired == 0 {
		localCacheConfig.NullValueCacheExpired = DefaultNullValueCacheExpired
	}
	if localCacheConfig.CacheExpiredRemove == 0 {
		localCacheConfig.CacheExpiredRemove = DefaultCacheExpiredRemove
	}

	localCache.localCacheConfig = localCacheConfig
	return localCache.localCacheConfig
}
