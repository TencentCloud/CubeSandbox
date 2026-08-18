// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/localcache/util"
)

func TestConcurrentGetAndRefreshOnSameKey(t *testing.T) {
	var loads int64
	localCache := NewCache("race-probe",
		func(ctx context.Context, key string) (interface{}, bool, error) {
			atomic.AddInt64(&loads, 1)
			return RandString(16), true, nil
		},
		&LocalCacheConfig{
			LowCacheSize:       1000000,
			HighCacheSize:      2000000,
			Expired:            time.Millisecond,
			AsyncRefreshBefore: time.Millisecond,
			MaxAsyncRefreshNum: 100,
			ExpiredUse:         true,
		})
	defer localCache.Destroy()

	ctx := context.Background()
	const readers = 32
	const iterations = 300

	var wg sync.WaitGroup
	wg.Add(readers)
	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				v, found, err := localCache.Get(ctx, "hot-key")
				if err != nil {
					t.Errorf("Get: %v", err)
					return
				}
				if found {
					if _, ok := v.(string); !ok {
						t.Errorf("Get returned %T, want string; a torn interface read", v)
						return
					}
				}
			}
		}()
	}
	wg.Wait()

	if atomic.LoadInt64(&loads) == 0 {
		t.Fatal("loader never ran; the test did not exercise the refresh path")
	}
}

func TestDestroyIsIdempotent(t *testing.T) {
	localCache := NewCache("destroy-probe",
		func(ctx context.Context, key string) (interface{}, bool, error) {
			return "v", true, nil
		},
		&LocalCacheConfig{LowCacheSize: 1000, HighCacheSize: 2000, Expired: time.Minute})

	localCache.Destroy()
	localCache.Destroy()
}

func TestConcurrentDestroyDoesNotRaceWithBackgroundLoops(t *testing.T) {
	localCache := NewCache("destroy-race-probe",
		func(ctx context.Context, key string) (interface{}, bool, error) {
			return "v", true, nil
		},
		&LocalCacheConfig{LowCacheSize: 1000, HighCacheSize: 2000, Expired: time.Minute})

	var wg sync.WaitGroup
	wg.Add(4)
	for i := 0; i < 4; i++ {
		go func() {
			defer wg.Done()
			localCache.Destroy()
		}()
	}
	wg.Wait()
}

func TestDestroySnapshotDoesNotRaceWithInFlightRefresh(t *testing.T) {
	localCache := NewCache("destroy-snapshot-probe",
		func(ctx context.Context, key string) (interface{}, bool, error) {
			return RandString(16), true, nil
		},
		&LocalCacheConfig{
			LowCacheSize:       1000000,
			HighCacheSize:      2000000,
			Expired:            time.Millisecond,
			AsyncRefreshBefore: time.Millisecond,
			MaxAsyncRefreshNum: 100,
			ExpiredUse:         true,
			OpenCacheFile:      true,
			LoadFileName:       filepath.Join(t.TempDir(), "cache.gob"),
		})

	ctx := context.Background()
	const keys = 64
	for i := 0; i < keys; i++ {
		if _, _, err := localCache.Get(ctx, "k"+strconv.Itoa(i)); err != nil {
			t.Fatalf("seed Get: %v", err)
		}
	}

	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(8)
	for r := 0; r < 8; r++ {
		go func(r int) {
			defer wg.Done()
			for i := 0; !stop.Load(); i++ {
				if _, _, err := localCache.Get(ctx, "k"+strconv.Itoa((i+r)%keys)); err != nil {
					t.Errorf("Get: %v", err)
					return
				}
			}
		}(r)
	}

	time.Sleep(50 * time.Millisecond)
	localCache.Destroy()
	stop.Store(true)
	wg.Wait()
}

func frontKey(t *testing.T, c *LocalCache) string {
	t.Helper()
	c.Lock()
	defer c.Unlock()
	front := c.valueList.Front()
	if front == nil {
		t.Fatal("value list is empty")
	}
	return front.Value.(*util.CacheValue).Key
}

func TestFailingRefreshDoesNotPromoteTheEntry(t *testing.T) {
	var fail atomic.Bool
	localCache := NewCache("lru-demotion-probe",
		func(ctx context.Context, key string) (interface{}, bool, error) {
			if fail.Load() && key == "a" {
				return nil, false, errors.New("loader is down")
			}
			return "v-" + key, true, nil
		},
		&LocalCacheConfig{
			LowCacheSize:       1000000,
			HighCacheSize:      2000000,
			Expired:            time.Millisecond,
			ExpiredUse:         false,
			DemotionExpiredUse: false,
		})
	defer localCache.Destroy()

	ctx := context.Background()
	for _, k := range []string{"a", "b"} {
		if _, _, err := localCache.Get(ctx, k); err != nil {
			t.Fatalf("seed Get(%s): %v", k, err)
		}
	}
	if got := frontKey(t, localCache); got != "a" {
		t.Fatalf("front is %q before the probe, want a", got)
	}

	fail.Store(true)
	time.Sleep(5 * time.Millisecond)

	if _, _, err := localCache.Get(ctx, "a"); err == nil {
		t.Fatal("Get(a) succeeded while the loader was failing")
	}
	if got := frontKey(t, localCache); got != "a" {
		t.Fatalf("a failing entry was promoted: front is %q, want a to stay evictable", got)
	}
}

func TestSuccessfulHitPromotesTheEntry(t *testing.T) {
	localCache := NewCache("lru-promote-probe",
		func(ctx context.Context, key string) (interface{}, bool, error) {
			return "v-" + key, true, nil
		},
		&LocalCacheConfig{
			LowCacheSize:  1000000,
			HighCacheSize: 2000000,
			Expired:       time.Hour,
		})
	defer localCache.Destroy()

	ctx := context.Background()
	for _, k := range []string{"a", "b"} {
		if _, _, err := localCache.Get(ctx, k); err != nil {
			t.Fatalf("seed Get(%s): %v", k, err)
		}
	}
	if got := frontKey(t, localCache); got != "a" {
		t.Fatalf("front is %q before the probe, want a", got)
	}

	if _, _, err := localCache.Get(ctx, "a"); err != nil {
		t.Fatalf("Get(a): %v", err)
	}
	if got := frontKey(t, localCache); got != "b" {
		t.Fatalf("a fresh hit did not promote: front is %q, want b", got)
	}
}
