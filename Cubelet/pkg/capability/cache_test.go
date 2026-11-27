// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package capability

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func artifactResolved() Resolved {
	return Resolved{
		Capabilities: Set{CapUpdateSnapshotCapture: LevelInitial},
		Source:       SourceArtifact,
		Version:      "v0.8.0",
	}
}

// A second lookup for the same build must not touch the resolver, which is what
// reads the artifact from disk.
func TestCacheServesARepeatedLookupWithoutResolving(t *testing.T) {
	cache := NewCache()
	calls := 0
	resolve := func() Resolved {
		calls++
		return artifactResolved()
	}

	first := cache.Get("cube-shim", "/c/v0.8.0/bin/shim", "v0.8.0", resolve)
	second := cache.Get("cube-shim", "/c/v0.8.0/bin/shim", "v0.8.0", resolve)

	require.Equal(t, 1, calls, "the resolver runs once per key")
	require.Equal(t, SourceArtifact, first.Source)
	require.Equal(t, SourceArtifact, second.Source)
	require.True(t, second.Capabilities.Has(CapUpdateSnapshotCapture, LevelInitial))
}

// The same version installed in two trees has two declarations, so the path is
// part of the key and one tree's answer must not stand for the other's.
func TestCacheKeysOnTheArtifactPathNotJustTheVersion(t *testing.T) {
	cache := NewCache()
	released := func() Resolved { return artifactResolved() }
	live := func() Resolved {
		return Resolved{
			Capabilities: Set{CapUpdateSnapshotCapture: LevelInitial, CapGuestFSFreeze: LevelInitial},
			Source:       SourceArtifact,
			Version:      "v0.8.0",
		}
	}

	fromReleased := cache.Get("cube-shim", "/component_versions/v0.8.0/bin/shim", "v0.8.0", released)
	fromLive := cache.Get("cube-shim", "/cubetoolbox/cube-shim/bin/shim", "v0.8.0", live)

	require.False(t, fromReleased.Capabilities.Has(CapGuestFSFreeze, LevelInitial))
	require.True(t, fromLive.Capabilities.Has(CapGuestFSFreeze, LevelInitial),
		"a different path must resolve independently")
}

func TestCacheKeysOnTheComponent(t *testing.T) {
	cache := NewCache()
	shim := func() Resolved { return artifactResolved() }
	agent := func() Resolved {
		return Resolved{
			Capabilities: Set{CapGuestFSFreeze: LevelInitial},
			Source:       SourceArtifact,
			Version:      "v0.8.0",
		}
	}

	cache.Get("cube-shim", "/same/path", "v0.8.0", shim)
	got := cache.Get("cube-agent", "/same/path", "v0.8.0", agent)

	require.True(t, got.Capabilities.Has(CapGuestFSFreeze, LevelInitial))
}

// SourceNone is not cached: it can be a transient read failure, and caching it
// would make one bad read permanent for every sandbox on that build.
func TestCacheDoesNotStoreAnUnknownAnswer(t *testing.T) {
	cache := NewCache()
	calls := 0
	unknownThenKnown := func() Resolved {
		calls++
		if calls == 1 {
			return Resolved{Source: SourceNone, Version: "v0.8.0"}
		}
		return artifactResolved()
	}

	first := cache.Get("cube-shim", "/c/v0.8.0/bin/shim", "v0.8.0", unknownThenKnown)
	require.Equal(t, SourceNone, first.Source)

	second := cache.Get("cube-shim", "/c/v0.8.0/bin/shim", "v0.8.0", unknownThenKnown)
	require.Equal(t, 2, calls, "an unknown answer must be retried")
	require.Equal(t, SourceArtifact, second.Source)
}

// The frozen table is a positive answer and is cached like an artifact one.
func TestCacheStoresATableAnswer(t *testing.T) {
	cache := NewCache()
	calls := 0
	resolve := func() Resolved {
		calls++
		return Resolved{
			Capabilities: Set{CapUpdateSnapshotCapture: LevelInitial},
			Source:       SourceLegacyTable,
			Version:      "v0.7.3",
		}
	}

	cache.Get("cube-shim", "", "v0.7.3", resolve)
	got := cache.Get("cube-shim", "", "v0.7.3", resolve)

	require.Equal(t, 1, calls)
	require.Equal(t, SourceLegacyTable, got.Source)
}

// A cached set is copied on the way in and on the way out, so one caller cannot
// change what the next caller sees.
func TestCacheHandsOutIndependentCopies(t *testing.T) {
	cache := NewCache()
	source := artifactResolved()
	cache.Get("cube-shim", "/c/v0.8.0/bin/shim", "v0.8.0", func() Resolved { return source })

	// Mutating the value the resolver returned must not change the cached entry.
	source.Capabilities[CapErrorsCoded] = LevelInitial

	first := cache.Get("cube-shim", "/c/v0.8.0/bin/shim", "v0.8.0", func() Resolved {
		t.Fatal("must be a cache hit")
		return Resolved{}
	})
	require.False(t, first.Capabilities.Has(CapErrorsCoded, LevelInitial))

	// Mutating what a caller received must not change the cached entry either.
	first.Capabilities[CapUpdateRollbackSnapshot] = LevelInitial
	second := cache.Get("cube-shim", "/c/v0.8.0/bin/shim", "v0.8.0", func() Resolved {
		t.Fatal("must be a cache hit")
		return Resolved{}
	})
	require.False(t, second.Capabilities.Has(CapUpdateRollbackSnapshot, LevelInitial))
}

// A nil cache still answers correctly, so callers need no nil check.
func TestNilCacheResolvesEveryTime(t *testing.T) {
	var cache *Cache
	calls := 0
	resolve := func() Resolved {
		calls++
		return artifactResolved()
	}

	require.Equal(t, SourceArtifact, cache.Get("cube-shim", "/p", "v0.8.0", resolve).Source)
	require.Equal(t, SourceArtifact, cache.Get("cube-shim", "/p", "v0.8.0", resolve).Source)
	require.Equal(t, 2, calls)
}

// The bound only stops new entries; existing ones keep being served.
func TestCacheStopsAdmittingPastTheLimit(t *testing.T) {
	cache := NewCache()
	for i := 0; i < maxCachedEntries; i++ {
		cache.Get("cube-shim", "/c", string(rune('a'+i)), artifactResolved)
	}

	overflowed := 0
	over := cache.Get("cube-shim", "/c", "overflow", func() Resolved {
		overflowed++
		return artifactResolved()
	})
	require.Equal(t, 1, overflowed, "an over-limit answer is still returned")
	require.Equal(t, SourceArtifact, over.Source)

	// Still recomputed next time, because it was never stored.
	cache.Get("cube-shim", "/c", "overflow", func() Resolved {
		overflowed++
		return artifactResolved()
	})
	require.Equal(t, 2, overflowed)

	// An entry admitted before the limit is still served from the cache.
	cache.Get("cube-shim", "/c", "a", func() Resolved {
		t.Fatal("a stored entry must still be a hit")
		return Resolved{}
	})
}
