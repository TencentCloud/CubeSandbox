// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package pmem

import (
	"context"
	"sync"
)

// Pins cover the interval before a new sandbox appears in the runtime index.
type cacheLease struct {
	users    int
	evicting chan struct{}
}

var cacheLeases = struct {
	sync.Mutex
	entries map[string]*cacheLease
}{entries: make(map[string]*cacheLease)}

func PinArtifact(ctx context.Context, instanceType, artifactID string) (func(), error) {
	key := GetRawImageFilePath(instanceType, artifactID)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cacheLeases.Lock()
		entry := cacheLeases.entries[key]
		if entry == nil {
			entry = &cacheLease{}
			cacheLeases.entries[key] = entry
		}
		if entry.evicting == nil {
			entry.users++
			cacheLeases.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					cacheLeases.Lock()
					defer cacheLeases.Unlock()
					entry.users--
					if entry.users == 0 {
						delete(cacheLeases.entries, key)
					}
				})
			}, nil
		}
		wait := entry.evicting
		cacheLeases.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-wait:
		}
	}
}

// TryEvictArtifact skips borrowers. New borrowers wait for unlink to finish
// and then ensure/download the file before proceeding.
func TryEvictArtifact(instanceType, artifactID string) (func(), bool) {
	key := GetRawImageFilePath(instanceType, artifactID)
	cacheLeases.Lock()
	defer cacheLeases.Unlock()
	if cacheLeases.entries[key] != nil {
		return nil, false
	}
	return beginEvictionLocked(key), true
}

// beginEvictionLocked requires cacheLeases to be locked and the key to be free.
func beginEvictionLocked(key string) func() {
	entry := &cacheLease{evicting: make(chan struct{})}
	cacheLeases.entries[key] = entry
	var once sync.Once
	return func() {
		once.Do(func() {
			cacheLeases.Lock()
			defer cacheLeases.Unlock()
			delete(cacheLeases.entries, key)
			close(entry.evicting)
		})
	}
}

// EvictArtifact waits for another eviction, but reports active borrowers
// separately so callers do not mistake a short GC lease for a running sandbox.
func EvictArtifact(ctx context.Context, instanceType, artifactID string) (func(), bool, error) {
	key := GetRawImageFilePath(instanceType, artifactID)
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		cacheLeases.Lock()
		entry := cacheLeases.entries[key]
		if entry == nil {
			release := beginEvictionLocked(key)
			cacheLeases.Unlock()
			return release, true, nil
		}
		if entry.users > 0 {
			cacheLeases.Unlock()
			return nil, false, nil
		}
		wait := entry.evicting
		cacheLeases.Unlock()
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-wait:
		}
	}
}
