// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"context"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
