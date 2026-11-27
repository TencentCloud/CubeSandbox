// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package capability

import "sync"

// maxCachedEntries bounds the cache. A node carries a handful of component
// versions, so this is only a guard against an unbounded set of paths: past the
// limit results are computed and not stored, which stays correct.
const maxCachedEntries = 64

// cacheKey identifies one resolution. The artifact path is part of the key
// because the same version can be installed in more than one tree (the released
// inventory and the live toolbox), and each tree publishes its own declaration.
// Keying on the version alone would let one tree's answer stand for another's.
type cacheKey struct {
	component string
	path      string
	version   string
}

// Cache holds resolutions keyed by the inputs that determine them. Capabilities
// are a pure function of the artifact and the pinned version, both fixed when a
// sandbox is created, so one entry serves every sandbox using that build.
//
// Only positive answers are stored. SourceNone can mean "this version's order is
// not provable" (permanent) but also "the file could not be read just now"
// (transient); caching it would turn a transient failure into a permanent one.
//
// The zero value is not usable; call NewCache.
type Cache struct {
	mu       sync.RWMutex
	resolved map[cacheKey]Resolved
}

// NewCache returns an empty cache.
func NewCache() *Cache {
	return &Cache{resolved: make(map[cacheKey]Resolved)}
}

// Get returns the resolution for one component at one path and version,
// computing it with resolve on a miss. resolve does all the work, including
// locating the artifact inside path, so a hit costs no filesystem access.
func (c *Cache) Get(component, path, version string, resolve func() Resolved) Resolved {
	if c == nil {
		return resolve()
	}
	key := cacheKey{component: component, path: path, version: version}

	c.mu.RLock()
	cached, ok := c.resolved[key]
	c.mu.RUnlock()
	if ok {
		// Copy: the set is handed to callers that may keep or mutate it.
		return Resolved{
			Capabilities: cached.Capabilities.Clone(),
			Source:       cached.Source,
			Version:      cached.Version,
		}
	}

	resolved := resolve()
	if resolved.Source == SourceArtifact || resolved.Source == SourceLegacyTable {
		c.store(key, resolved)
	}
	return resolved
}

func (c *Cache) store(key cacheKey, resolved Resolved) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.resolved) >= maxCachedEntries {
		if _, exists := c.resolved[key]; !exists {
			return
		}
	}
	c.resolved[key] = Resolved{
		Capabilities: resolved.Capabilities.Clone(),
		Source:       resolved.Source,
		Version:      resolved.Version,
	}
}
