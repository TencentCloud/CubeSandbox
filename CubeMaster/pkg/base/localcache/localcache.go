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
	"github.com/tencentcloud/CubeSandbox/cubelog"
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
	if localCache != nil {
		CubeLog.Infof("LruCache(%s) Destroy", localCache.name)
		if localCache.chCacheExit != nil {
			if localCache.localCacheConfig.OpenCacheFile {
				localCache.saveFile(localCache.localCacheConfig.LoadFileName)
			}
			localCache.cache.Flush()
			close(localCache.chCacheExit)
			localCache.waitGroup.Wait()
			localCache.chCacheExit = nil
		}
	}
}

func (localCache *LocalCache) Get(ctx context.Context, key string) (interface{}, bool, error) {
	item, found := localCache.cache.Get(key)
	if found {
		element := item.(*list.Element)
		itm := element.Value.(*util.CacheValue)

		if time.Now().Add(-itm.Expired).After(time.Unix(itm.LastAccess, 0)) {

			if !localCache.localCacheConfig.ExpiredUse {
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
		localCache.valueList.MoveToBack(element)
		localCache.Unlock()
		itm := element.Value.(*util.CacheValue)
		atomic.AddInt64(&localCache.curCacheSize, -itm.Size())
		itm.Value = val
		itm.Expired = expired
		itm.LastAccess = time.Now().Unix()
		atomic.AddInt64(&localCache.curCacheSize, itm.Size())
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

	if localCache.curCacheSize >= localCache.localCacheConfig.HighCacheSize {
		localCache.chShrinkCache <- true
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
				localCache.consecutiveFailNum = 0
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
	defer CubeLog.Infof("Cache Shrink Process goroutine End")
	ticker := time.NewTicker(time.Duration(15) * time.Minute)
	for {
		select {
		case <-localCache.chShrinkCache:
			curTime := time.Now()
			curCacheSize := localCache.curCacheSize
			var shrinkNum, shrinkSize, size int64
			for {
				if localCache.curCacheSize > localCache.localCacheConfig.LowCacheSize {
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
						localCache.localCacheConfig.ExpiredUse = true
					}
				} else {

					if localCache.localCacheConfig.DemotionExpiredUse && localCache.localCacheConfig.ExpiredUse {
						localCache.localCacheConfig.ExpiredUse = false
					}
				}
			}
		}
	}
}

func (localCache *LocalCache) saveFile(file string) {
	for key, item := range localCache.cache.Items() {
		element := item.Object.(*list.Element)
		itm := element.Value.(*util.CacheValue)
		localCache.cache.Set(key, itm, -1)
	}

	_ = localCache.cache.SaveFile(file)
}

func (localCache *LocalCache) loadFile(file string) {
	gob.Register(&util.CacheValue{})
	_ = localCache.cache.LoadFile(file)
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
