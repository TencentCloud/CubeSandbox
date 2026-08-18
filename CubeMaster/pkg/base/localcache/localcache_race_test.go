// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"context"
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
